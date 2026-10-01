package usage

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	sqlite "modernc.org/sqlite"
)

// The JSONL remains authoritative. Only readers maintain this disposable index;
// Append never waits for SQLite, including when another process owns its writer.
func usageIndexPath() string {
	return filepath.Join(filepath.Dir(catalog.CachePath()), "usage-index.sqlite")
}

const indexSchema = `
CREATE TABLE IF NOT EXISTS checkpoint (singleton INTEGER PRIMARY KEY CHECK(singleton=1), path TEXT NOT NULL, size INTEGER NOT NULL, modified INTEGER NOT NULL, off INTEGER NOT NULL, hash TEXT NOT NULL, meta TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS calls (
 seq INTEGER PRIMARY KEY, stamp INTEGER NOT NULL, day INTEGER NOT NULL, slot INTEGER NOT NULL, route INTEGER NOT NULL,
 agent TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL,
 failed INTEGER NOT NULL, rejected INTEGER NOT NULL,
 input INTEGER NOT NULL, output INTEGER NOT NULL, cache_read INTEGER NOT NULL, cache_write INTEGER NOT NULL, reasoning INTEGER NOT NULL,
 cost REAL NOT NULL, priced INTEGER NOT NULL, unpriced INTEGER NOT NULL,
 timed INTEGER NOT NULL, ttft INTEGER NOT NULL, decode_ms INTEGER NOT NULL, decode_out INTEGER NOT NULL,
 session TEXT NOT NULL, native_session TEXT NOT NULL, request_id TEXT NOT NULL, millis INTEGER NOT NULL,
 search TEXT NOT NULL, raw TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS calls_time ON calls(stamp DESC,seq DESC);
CREATE INDEX IF NOT EXISTS calls_agent ON calls(agent,stamp DESC,seq DESC);
CREATE INDEX IF NOT EXISTS calls_provider ON calls(provider,stamp DESC,seq DESC);
CREATE INDEX IF NOT EXISTS calls_model ON calls(model,stamp DESC,seq DESC);
CREATE INDEX IF NOT EXISTS calls_route ON calls(route,stamp DESC,seq DESC);
CREATE INDEX IF NOT EXISTS calls_match ON calls(stamp,seq) WHERE session<>'' OR native_session<>'' OR request_id<>'';
CREATE TABLE IF NOT EXISTS rollup (
 stamp INTEGER NOT NULL, slot INTEGER NOT NULL, day INTEGER NOT NULL,
 agent TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, failed INTEGER NOT NULL, rejected INTEGER NOT NULL,
 calls INTEGER NOT NULL, input INTEGER NOT NULL, output INTEGER NOT NULL, cache_read INTEGER NOT NULL, cache_write INTEGER NOT NULL, reasoning INTEGER NOT NULL,
 cost REAL NOT NULL, unpriced INTEGER NOT NULL, timed INTEGER NOT NULL, ttft INTEGER NOT NULL, decode_ms INTEGER NOT NULL, decode_out INTEGER NOT NULL,
 PRIMARY KEY(slot,agent,provider,model,failed,rejected)) WITHOUT ROWID;
PRAGMA user_version=3;`

func openUsageIndex(meta string, price func(Record, string) Row) (*sql.DB, error) {
	path := usageIndexPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	file.Close()
	for attempt := 0; attempt < 2; attempt++ {
		u := url.URL{Scheme: "file", Path: path}
		q := u.Query()
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Add("_pragma", "cache_size(-2048)")
		q.Set("_txlock", "immediate")
		u.RawQuery = q.Encode()
		db, err := sql.Open("sqlite", u.String())
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		err = syncUsageIndex(db, meta, price)
		if err == nil {
			return db, nil
		}
		db.Close()
		var se *sqlite.Error
		if attempt == 0 && errors.As(err, &se) && (se.Code()&255 == 11 || se.Code()&255 == 26) {
			// All connections opened here are closed before deleting corrupt cache files.
			// The source log is never removed, and its next read rebuilds this cache.
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if e := os.Remove(path + suffix); e != nil && !os.IsNotExist(e) {
					return nil, e
				}
			}
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("cannot rebuild usage index")
}

func syncUsageIndex(db *sql.DB, meta string, price func(Record, string) Row) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 3 {
		if _, err = tx.ExecContext(ctx, "DROP TABLE IF EXISTS calls; DROP TABLE IF EXISTS rollup; DROP TABLE IF EXISTS checkpoint;"); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, indexSchema); err != nil {
		return err
	}
	var oldPath, hash, oldMeta string
	var size, modified, off int64
	err = tx.QueryRowContext(ctx, "SELECT path,size,modified,off,hash,meta FROM checkpoint WHERE singleton=1").Scan(&oldPath, &size, &modified, &off, &hash, &oldMeta)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	info, err := os.Stat(Path())
	if os.IsNotExist(err) {
		if _, err = tx.ExecContext(ctx, "DELETE FROM calls; DELETE FROM rollup; DELETE FROM checkpoint;"); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	same := oldPath == Path() && oldMeta == meta
	if same && size == info.Size() && modified == info.ModTime().UnixNano() {
		return tx.Commit()
	}
	continued := same && info.Size() > size && hash != "" && recordHash(Path(), size) == hash
	if !continued {
		if _, err = tx.ExecContext(ctx, "DELETE FROM calls; DELETE FROM rollup;"); err != nil {
			return err
		}
		off = 0
	}
	file, err := os.Open(Path())
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err = file.Seek(off, io.SeekStart); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO calls VALUES ("+strings.TrimSuffix(strings.Repeat("?,", 28), ",")+")")
	if err != nil {
		return err
	}
	defer stmt.Close()
	importOffset := off
	beforeHash := recordHash(Path(), info.Size())
	reader := bufio.NewReader(io.LimitReader(file, info.Size()-off))
	renamed := provider.Renamed()
	for {
		b, e := reader.ReadBytes('\n')
		if e != nil {
			if e != io.EOF {
				return e
			}
			break
		}
		seq := off
		off += int64(len(b))
		var r Record
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		if id, ok := renamed[r.Provider]; ok {
			r.Provider = id
		}
		row := price(r, "")
		var totals Totals
		totals.addRow(row)
		fields := []string{row.Requested, row.Model, row.Served, row.Provider, row.Host, row.Session, row.Effort, row.SessionProvider, row.SessionAccount, row.ProviderKeyID, row.ProviderKeyName}
		for i := range fields {
			fields[i] = strings.ToLower(fields[i])
		}
		search, _ := json.Marshal(fields)
		stamp := int64(0)
		if !r.Time.IsZero() {
			stamp = r.Time.UnixNano()
		}
		_, err = stmt.ExecContext(ctx, seq, stamp, calendarDays(time.Unix(0, 0).UTC(), r.Time.In(time.Local)), indexSlot(r.Time), r.RouteID, row.Agent, row.Provider, row.Model, row.Failed(), row.IsRejected(), totals.Input, totals.Output, totals.CacheRead, totals.CacheWrite, totals.Reasoning, totals.Cost, row.Priced, totals.Unpriced, totals.Timed, totals.TTFT, totals.DecodeMs, totals.DecodeOut, r.Session, r.NativeSession, r.RequestID, r.Millis, string(search), strings.TrimSpace(string(b)))
		if err != nil {
			return err
		}
	}
	// Only newly imported records update the SQL rollups. No raw rows are
	// decoded for the common period/model/provider/agent aggregates.
	updates := []string{"stamp=MIN(rollup.stamp,excluded.stamp)"}
	for _, column := range []string{"calls", "input", "output", "cache_read", "cache_write", "reasoning", "cost", "unpriced", "timed", "ttft", "decode_ms", "decode_out"} {
		updates = append(updates, column+"=rollup."+column+"+excluded."+column)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO rollup SELECT MIN(stamp),slot,day,agent,provider,model,failed,rejected,COUNT(*),SUM(input),SUM(output),SUM(cache_read),SUM(cache_write),SUM(reasoning),SUM(cost),SUM(unpriced),SUM(timed),SUM(ttft),SUM(decode_ms),SUM(decode_out) FROM calls WHERE seq>=? GROUP BY slot,day,agent,provider,model,failed,rejected ON CONFLICT(slot,agent,provider,model,failed,rejected) DO UPDATE SET "+strings.Join(updates, ","), importOffset)
	if err != nil {
		return err
	}
	current, err := os.Stat(Path())
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(opened, current) || current.Size() < info.Size() {
		return fmt.Errorf("usage log replaced during indexing")
	}
	hash = recordHash(Path(), info.Size())
	if hash != beforeHash {
		return fmt.Errorf("usage log changed during indexing")
	}
	_, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO checkpoint VALUES(1,?,?,?,?,?,?)", Path(), info.Size(), info.ModTime().UnixNano(), off, hash, meta)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func indexWhere(since time.Time, f Filter, accepted bool) (string, []any) {
	terms := []string{"1=1"}
	var args []any
	if !since.IsZero() {
		terms = append(terms, "stamp>=?")
		args = append(args, since.UnixNano())
	}
	for _, x := range []struct {
		column string
		value  any
		on     bool
	}{{"route", f.RouteID, f.RouteID != 0}, {"model", f.Model, f.Model != ""}, {"agent", f.Agent, f.Agent != ""}, {"provider", f.Provider, f.Provider != ""}} {
		if x.on {
			terms = append(terms, x.column+"=?")
			args = append(args, x.value)
		}
	}
	if f.Failed {
		terms = append(terms, "failed=1")
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		terms = append(terms, "EXISTS(SELECT 1 FROM json_each(calls.search) WHERE instr(value,?)>0)")
		args = append(args, q)
	}
	if accepted {
		terms = append(terms, "rejected=0")
	}
	return strings.Join(terms, " AND "), args
}

const indexTotals = `COUNT(*),COALESCE(SUM(failed),0),COALESCE(SUM(input),0),COALESCE(SUM(output),0),COALESCE(SUM(cache_read),0),COALESCE(SUM(cache_write),0),COALESCE(SUM(reasoning),0),COALESCE(SUM(cost),0),COALESCE(SUM(unpriced),0),COALESCE(SUM(timed),0),COALESCE(SUM(ttft),0),COALESCE(SUM(decode_ms),0),COALESCE(SUM(decode_out),0)`

func totalDest(t *Totals) []any {
	return []any{&t.Calls, &t.Errors, &t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.Reasoning, &t.Cost, &t.Unpriced, &t.Timed, &t.TTFT, &t.DecodeMs, &t.DecodeOut}
}
func addTotals(a *Totals, b Totals) {
	a.Calls += b.Calls
	a.Errors += b.Errors
	a.Input += b.Input
	a.Output += b.Output
	a.CacheRead += b.CacheRead
	a.CacheWrite += b.CacheWrite
	a.Reasoning += b.Reasoning
	a.Cost += b.Cost
	a.Unpriced += b.Unpriced
	a.Timed += b.Timed
	a.TTFT += b.TTFT
	a.DecodeMs += b.DecodeMs
	a.DecodeOut += b.DecodeOut
}

func indexShares(db indexReader, since time.Time, f Filter, dimension string) (map[string]*Share, error) {
	where, args := indexWhere(since, f, true)
	source, totals, _ := indexAggregateSource(f)
	rows, err := db.Query("SELECT "+dimension+","+totals+" FROM "+source+" WHERE "+where+" GROUP BY "+dimension, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Share{}
	for rows.Next() {
		s := &Share{}
		if err = rows.Scan(append([]any{&s.ID}, totalDest(&s.Totals)...)...); err != nil {
			return nil, err
		}
		out[s.ID] = s
	}
	return out, rows.Err()
}

func indexSlot(t time.Time) int64 {
	local := t.In(time.Local)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
	return day.Add(time.Duration(int64(t.Sub(day)/time.Hour)) * time.Hour).UnixNano()
}

func indexAggregateSource(f Filter) (string, string, string) {
	if f.RouteID != 0 || strings.TrimSpace(f.Query) != "" {
		return "calls", indexTotals, "COUNT(*)"
	}
	totals := strings.Replace(indexTotals, "COUNT(*)", "COALESCE(SUM(calls),0)", 1)
	totals = strings.Replace(totals, "SUM(failed)", "SUM(failed*calls)", 1)
	return "rollup", totals, "COALESCE(SUM(calls),0)"
}
