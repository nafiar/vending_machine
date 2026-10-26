package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vending-machine/internal/board"
)

const id1 = "11111111-1111-4111-8111-111111111111"
const id2 = "22222222-2222-4222-8222-222222222222"

func TestPutSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	j, problems, err := Open(dir)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	if err := j.Put(Entry{TxID: id1, Slot: 3, Stage: Vending}); err != nil {
		t.Fatal(err)
	}
	if err := j.Put(Entry{TxID: id2, Slot: 1, Stage: Paying}); err != nil {
		t.Fatal(err)
	}
	first, _ := j.Get(id1)
	res := board.Result{OK: true, Slot: 3, Code: "AB", AckMs: 70, EndMs: 2450}
	if err := j.Put(Entry{TxID: id1, Slot: 3, Stage: Result, Status: "DISPENSED", Board: &res}); err != nil {
		t.Fatal(err)
	}

	again, problems, err := Open(dir)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	e, ok := again.Get(id1)
	if !ok || e.Stage != Result || e.Status != "DISPENSED" || e.Board.EndMs != 2450 || !e.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("reopened: %+v", e)
	}
	if list := again.List(); len(list) != 2 || list[0].TxID != id1 {
		t.Fatalf("oldest first: %+v", list)
	}
	if err := again.Delete(id1); err != nil || again.Len() != 1 {
		t.Fatal(err, again.Len())
	}
	if _, err := os.Stat(filepath.Join(dir, id1+".json")); !os.IsNotExist(err) {
		t.Fatal("the file must be gone")
	}
}

func TestDamagedFilesAreSetAside(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, id1+".json"), []byte("{not json"), 0o644)
	os.WriteFile(filepath.Join(dir, id2+".json.tmp"), []byte("half"), 0o644)
	j, problems, err := Open(dir)
	if err != nil || j.Len() != 0 || len(problems) != 1 || !strings.Contains(problems[0], ".bad") {
		t.Fatal(err, j.Len(), problems)
	}
	if _, err := os.Stat(filepath.Join(dir, id2+".json.tmp")); !os.IsNotExist(err) {
		t.Fatal("unfinished temporary files are removed")
	}
}

func TestOnlyTransactionIDsAreFileNames(t *testing.T) {
	j, _, _ := Open(t.TempDir())
	if err := j.Put(Entry{TxID: "../../evil", Stage: Paying}); err == nil {
		t.Fatal("a path must never become a file name")
	}
}
