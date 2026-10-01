package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func indexTestPrice(r Record, _ string) Row { r.Agent = AgentOf(r.Agent); return Row{Record: r} }
func indexCount(t *testing.T, want int) {
	t.Helper()
	db, err := openUsageIndex("test", indexTestPrice)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got int
	if err = db.QueryRow("SELECT COUNT(*) FROM calls").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("indexed %d, want %d", got, want)
	}
	if err = db.QueryRow("SELECT COALESCE(SUM(calls),0) FROM rollup").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("rollup %d, want %d", got, want)
	}
}
func TestSQLiteIndexAppendHalfLineAndReplacement(t *testing.T) {
	pageHome(t)
	now := time.Now().Round(0)
	r := Record{Time: now, Agent: "codex", Provider: "relay", Model: "m", Input: 10, RouteID: 81}
	Append(r)
	indexCount(t, 1)
	raw, _ := json.Marshal(r)
	file, err := os.OpenFile(Path(), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Write(raw[:len(raw)/2])
	file.Close()
	indexCount(t, 1)
	file, _ = os.OpenFile(Path(), os.O_APPEND|os.O_WRONLY, 0600)
	file.Write(append(raw[len(raw)/2:], '\n'))
	file.Close()
	indexCount(t, 2)
	// Alter the already indexed prefix and append: the old offset cannot be reused.
	body, _ := os.ReadFile(Path())
	for i := range body {
		if body[i] == '8' && i+1 < len(body) && body[i+1] == '1' {
			body[i] = '9'
			break
		}
	}
	os.WriteFile(Path(), append(body, append(raw, '\n')...), 0600)
	indexCount(t, 3)
	db, err := openUsageIndex("test", indexTestPrice)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM calls WHERE route=91").Scan(&n)
	db.Close()
	if n != 1 {
		t.Fatalf("prefix edit lost: %d", n)
	}
	replacement := Path() + ".replacement"
	os.WriteFile(replacement, append(raw, '\n'), 0600)
	if err = os.Rename(replacement, Path()); err != nil {
		t.Fatal(err)
	}
	indexCount(t, 1)
	os.Remove(Path())
	indexCount(t, 0)
}

func TestSQLiteIndexRebuildAndVersion(t *testing.T) {
	pageHome(t)
	Append(Record{Time: time.Now(), Provider: "relay", Model: "m", Agent: "codex", Input: 1})
	indexCount(t, 1)
	original, _ := os.ReadFile(Path())
	if err := os.Remove(usageIndexPath()); err != nil {
		t.Fatal(err)
	}
	indexCount(t, 1)
	db, err := openUsageIndex("test", indexTestPrice)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("PRAGMA user_version=999")
	db.Close()
	indexCount(t, 1)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(usageIndexPath() + suffix)
	}
	if err = os.WriteFile(usageIndexPath(), []byte("not a SQLite database"), 0600); err != nil {
		t.Fatal(err)
	}
	indexCount(t, 1)
	after, _ := os.ReadFile(Path())
	if !reflect.DeepEqual(original, after) {
		t.Fatal("cache rebuild changed the JSONL source")
	}
}

func TestSQLiteIndexIndependentWritersAndAppend(t *testing.T) {
	pageHome(t)
	for i := 0; i < 80; i++ {
		Append(Record{Time: time.Now(), Provider: "relay", Agent: "codex", Model: "m", Input: i})
	}
	indexCount(t, 80)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := openUsageIndex("test", indexTestPrice)
			if err == nil {
				db.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	db, err := openUsageIndex("test", indexTestPrice)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	done := make(chan struct{})
	go func() {
		Append(Record{Time: time.Now(), Provider: "relay", Agent: "codex", Model: "m", Input: 99})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("JSONL append waited on the SQLite writer")
	}
	tx.Rollback()
	indexCount(t, 81)
}

func TestSQLiteIndexUnavailableKeepsLedger(t *testing.T) {
	pageHome(t)
	Append(Record{Time: time.Now(), Provider: "relay", Agent: "codex", Model: "m", Input: 20})
	if err := os.Mkdir(usageIndexPath(), 0700); err != nil {
		t.Fatal(err)
	}
	got := QueryPage(All, Filter{}, 0, 100)
	if got.Total != 1 || got.Rows[0].Input != 20 {
		t.Fatalf("cache failure hid ledger: %+v", got)
	}
}

func TestSQLitePageFiltersAndMetadata(t *testing.T) {
	pageHome(t)
	now := time.Now().Round(0)
	for _, r := range []Record{
		{Time: now, Agent: "codex", Provider: "relay", Model: "gpt-5", Input: 10, RouteID: 1, ProviderKeyName: "ÄCCOUNT%_"},
		{Time: now, Agent: "codex", Provider: "relay", Model: "gpt-5-mini", Input: 20, RouteID: 2},
		{Time: now, Agent: "codex", Provider: "relay", Model: "m", Input: 0, Status: 200, Error: "upstream error", RouteID: 3},
		{Time: now, Agent: "codex", Model: "m", Input: 20, Status: 400, RouteID: 4},
	} {
		Append(r)
	}
	priceOf := pricer()
	basePrice := func(r Record, source string) Row {
		row := indexTestPrice(r, source)
		if price := priceOf(r); price != nil && r.Input+r.Output > 0 {
			row.Cost = price.Cost(r.Input, r.Output, r.CacheRead, r.CacheWrite)
			row.Priced = true
		}
		return row
	}
	db, err := openUsageIndex("test", basePrice)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, f := range []Filter{{}, {RouteID: 2}, {Failed: true}, {Model: "gpt-5"}, {Query: "äccount%_"}, {Query: "%_"}, {Query: "%missing"}} {
		got, err := indexedRequestPage(db, All, f, 0, 2, nil, basePrice)
		if err != nil {
			t.Fatal(err)
		}
		equalPage(t, got, pageFromLedger(All, f, 0, 2, LedgerOf(All, Filter{})))
	}
	db.Close()
	price := func(r Record, _ string) Row {
		return Row{Record: r, Cost: float64(r.Input), Priced: r.Input+r.Output > 0}
	}
	db, err = openUsageIndex("new prices", price)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := indexedRequestPage(db, All, Filter{}, 0, 100, nil, price)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sum.Cost != 30 {
		t.Fatalf("metadata invalidation kept old cost: %v", got.Sum.Cost)
	}
	if mode, err := os.Stat(filepath.Clean(usageIndexPath())); err != nil || mode.Mode().Perm() != 0600 {
		t.Fatalf("index permissions: %v %v", mode, err)
	}
}
