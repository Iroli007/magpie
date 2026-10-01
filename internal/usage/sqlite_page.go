package usage

import (
	"container/heap"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

type indexReader interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func indexedRequestPage(db *sql.DB, p Period, f Filter, offset, limit int, chunks []*rowChunk, price func(Record, string) Row) (RequestPage, error) {
	tx, err := db.Begin()
	if err != nil {
		return RequestPage{}, err
	}
	defer tx.Rollback()
	now := time.Now()
	since := p.Since(now)
	skip := visibleLocal(chunks)
	matched := map[rowRef]bool{}
	if len(chunks) > 0 {
		gateway := &rowChunk{}
		gs := since
		if !gs.IsZero() {
			gs = gs.Add(-24 * time.Hour)
		}
		where, args := indexWhere(gs, Filter{}, true)
		rows, e := tx.Query("SELECT stamp,agent,session,native_session,request_id,input,output,cache_read,cache_write,millis,failed FROM calls WHERE "+where+" AND (session<>'' OR native_session<>'' OR request_id<>'') ORDER BY seq", args...)
		if e != nil {
			return RequestPage{}, e
		}
		for rows.Next() {
			var stamp int64
			var failed bool
			r := Record{Provider: "indexed"}
			if e = rows.Scan(&stamp, &r.Agent, &r.Session, &r.NativeSession, &r.RequestID, &r.Input, &r.Output, &r.CacheRead, &r.CacheWrite, &r.Millis, &failed); e != nil {
				rows.Close()
				return RequestPage{}, e
			}
			if stamp != 0 {
				r.Time = time.Unix(0, stamp)
			}
			if failed {
				r.Status = 500
			}
			gateway.add(Row{Record: r}, "", int64(len(gateway.Rows)), false)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return RequestPage{}, e
		}
		matched = matchedLocal(gateway, chunks, skip, since)
	}
	visit := func(fn func(rowRef, Row)) {
		for _, c := range chunks {
			for i, pr := range c.Rows {
				ref := rowRef{c, i}
				if pr.Time.Before(since) || skip[ref] || matched[ref] {
					continue
				}
				fn(ref, c.row(i))
			}
		}
	}
	out := RequestPage{Rows: []Row{}, Agents: []string{}, Providers: []string{}, By: map[string][]Share{}}
	agents, providers := map[string]bool{}, map[string]bool{}
	periodWhere, periodArgs := indexWhere(since, Filter{}, false)
	for _, d := range []string{"agent", "provider"} {
		rows, e := tx.Query("SELECT DISTINCT "+d+" FROM rollup WHERE "+periodWhere, periodArgs...)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return out, e
			}
			if d == "agent" {
				agents[id] = true
			} else if id != "" {
				providers[id] = true
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
	}
	where, args := indexWhere(since, f, false)
	source, totalsSQL, countSQL := indexAggregateSource(f)
	if err = tx.QueryRow("SELECT "+countSQL+" FROM "+source+" WHERE "+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	accepted, acceptedArgs := indexWhere(since, f, true)
	if err = tx.QueryRow("SELECT "+totalsSQL+" FROM "+source+" WHERE "+accepted, acceptedArgs...).Scan(totalDest(&out.Sum)...); err != nil {
		return out, err
	}
	var firstStamp sql.NullInt64
	if err = tx.QueryRow("SELECT MIN(NULLIF(stamp,0)) FROM "+source+" WHERE "+accepted, acceptedArgs...).Scan(&firstStamp); err != nil {
		return out, err
	}
	var first time.Time
	if firstStamp.Valid {
		first = time.Unix(0, firstStamp.Int64)
	}
	groups, seriesGroups := map[string]map[string]*Share{}, map[string]map[string]*Share{}
	facetFilters := make([]Filter, len(Dimensions))
	for i, d := range Dimensions {
		g := f
		switch d {
		case "agent":
			g.Agent = ""
		case "provider":
			g.Provider = ""
		case "model":
			g.Model = ""
		}
		facetFilters[i] = g
		groups[d], err = indexShares(tx, since, g, d)
		if err != nil {
			return out, err
		}
		if g == f {
			seriesGroups[d] = groups[d]
			continue
		}
		seriesGroups[d], err = indexShares(tx, since, f, d)
		if err != nil {
			return out, err
		}
	}
	localCount := 0
	maxRows := out.Total
	for _, c := range chunks {
		maxRows += len(c.Rows)
	}
	prefix := 0
	if offset < maxRows {
		prefix = offset + min(limit, maxRows-offset)
	}
	selected := newestHeap{}
	visit(func(ref rowRef, r Row) {
		agents[r.Agent] = true
		if r.Provider != "" {
			providers[r.Provider] = true
		}
		if f.keeps(r.Record) {
			out.Total++
			localCount++
			if prefix > 0 && len(selected) < prefix {
				heap.Push(&selected, ref)
			} else if prefix > 0 && refNewer(ref, selected[0]) {
				selected[0] = ref
				heap.Fix(&selected, 0)
			}
			if !r.IsRejected() {
				out.Sum.addRow(r)
				if first.IsZero() || r.Time.Before(first) {
					first = r.Time
				}
				for i, d := range Dimensions {
					if facetFilters[i] != f {
						addShare(seriesGroups[d], r.key(d), r)
					}
				}
			}
		}
		if r.IsRejected() {
			return
		}
		for i, d := range Dimensions {
			if facetFilters[i].keeps(r.Record) {
				addShare(groups[d], r.key(d), r)
			}
		}
	})
	for id := range agents {
		out.Agents = append(out.Agents, id)
	}
	slices.Sort(out.Agents)
	for id := range providers {
		out.Providers = append(out.Providers, id)
	}
	slices.Sort(out.Providers)
	for _, d := range Dimensions {
		out.By[d] = sharesOf(groups[d])
	}
	if offset < out.Total {
		take := offset + min(limit, out.Total-offset)
		// At most localCount local rows can precede this page. Skip the gateway
		// prefix known to be before it, even for a deep offset or no local sources.
		start := max(0, offset-localCount)
		queryArgs := append(slices.Clone(args), take-start, start)
		rows, e := tx.Query("SELECT seq,raw FROM calls WHERE "+where+" ORDER BY stamp DESC,seq DESC LIMIT ? OFFSET ?", queryArgs...)
		if e != nil {
			return out, e
		}
		c := &rowChunk{}
		for rows.Next() {
			var seq int64
			var raw string
			if e = rows.Scan(&seq, &raw); e != nil {
				rows.Close()
				return out, e
			}
			var r Record
			if e = json.Unmarshal([]byte(raw), &r); e != nil {
				rows.Close()
				return out, e
			}
			c.add(price(r, ""), "", seq, false)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
		for i := range c.Rows {
			selected = append(selected, rowRef{c, i})
		}
		slices.SortFunc(selected, func(a, b rowRef) int {
			if refNewer(a, b) {
				return -1
			}
			if refNewer(b, a) {
				return 1
			}
			return 0
		})
		end := min(len(selected), take-start)
		for _, ref := range selected[min(offset-start, end):end] {
			out.Rows = append(out.Rows, ref.row())
		}
	}
	chartSince, bucket, base := timeline(p, now, first)
	out.Bucket = bucket
	out.Series = make([]SeriesPoint, len(base))
	for i, point := range base {
		out.Series[i] = SeriesPoint{Point: point, By: map[string]map[string]Part{}}
		for _, d := range Dimensions {
			out.Series[i].By[d] = map[string]Part{}
		}
	}
	if len(base) > 0 {
		// Civil-day ordinals keep day/week buckets correct across DST; hourly
		// buckets follow elapsed hours, as timeline and bucketIndex do.
		end := base[len(base)-1].Time.AddDate(0, 0, 1)
		bucketSQL := fmt.Sprintf("(day-%d)", calendarDays(time.Unix(0, 0).UTC(), chartSince))
		if bucket == "hour" {
			bucketSQL = fmt.Sprintf("((stamp-%d)/%d)", chartSince.UnixNano(), int64(time.Hour))
			end = base[len(base)-1].Time.Add(time.Hour)
		}
		if bucket == "week" {
			bucketSQL += "/7"
			end = base[len(base)-1].Time.AddDate(0, 0, 7)
		}
		chartArgs := append(slices.Clone(acceptedArgs), chartSince.UnixNano(), end.UnixNano())
		rows, e := tx.Query("SELECT "+bucketSQL+",agent,provider,model,"+totalsSQL+" FROM "+source+" WHERE "+accepted+" AND stamp>=? AND stamp<? GROUP BY 1,agent,provider,model", chartArgs...)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var i int
			var agent, provider, model string
			var t Totals
			if e = rows.Scan(append([]any{&i, &agent, &provider, &model}, totalDest(&t)...)...); e != nil {
				rows.Close()
				return out, e
			}
			if i < 0 || i >= len(base) {
				continue
			}
			point := &out.Series[i]
			addTotals(&point.Totals, t)
			for _, d := range Dimensions {
				id := model
				switch d {
				case "agent":
					id = agent
				case "provider":
					id = provider
				}
				part := point.By[d][id]
				part.Calls += t.Calls
				part.Tokens += t.AllTokens()
				part.Cost += t.Cost
				point.By[d][id] = part
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
	}
	visit(func(_ rowRef, r Row) {
		if !f.keeps(r.Record) || r.IsRejected() || r.Time.In(time.Local).Before(chartSince) {
			return
		}
		i := bucketIndex(bucket, chartSince, r.Time.In(time.Local))
		if i < 0 || i >= len(out.Series) {
			return
		}
		point := &out.Series[i]
		point.addRow(r)
		for _, d := range Dimensions {
			k := r.key(d)
			part := point.By[d][k]
			part.Calls++
			part.Tokens += r.Input + r.Output + r.CacheRead + r.CacheWrite
			if r.Priced {
				part.Cost += r.Cost
			}
			point.By[d][k] = part
		}
	})
	for _, d := range Dimensions {
		kept := map[string]bool{}
		for i, s := range sharesOf(seriesGroups[d]) {
			if i < seriesKeep {
				kept[s.ID] = true
			}
		}
		for i := range out.Series {
			for key := range out.Series[i].By[d] {
				if !kept[key] {
					delete(out.Series[i].By[d], key)
				}
			}
		}
	}
	return out, nil
}
func addShare(group map[string]*Share, key string, row Row) {
	s := group[key]
	if s == nil {
		s = &Share{ID: key}
		group[key] = s
	}
	s.addRow(row)
}
