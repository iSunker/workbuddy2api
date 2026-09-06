package pool

import (
	"testing"
	"time"

	"codebuddy2api/internal/cred"
)

func mkCred(uid string) *cred.Cred {
	return &cred.Cred{UID: uid, Nickname: uid, Token: "ck_" + uid, Kind: cred.KindAPIKey}
}

func TestPickRoundRobin(t *testing.T) {
	p := New("")
	p.Add(mkCred("a"))
	p.Add(mkCred("b"))

	first := p.Pick().UID
	second := p.Pick().UID
	third := p.Pick().UID

	// 错误数相同 → 最久未用优先，即严格交替
	if first == second {
		t.Fatalf("two consecutive picks returned %q (expected round-robin)", first)
	}
	if third != first {
		t.Fatalf("third pick = %q, want %q (round-robin)", third, first)
	}
}

func TestPickSkipsUnhealthy(t *testing.T) {
	p := New("")
	p.Add(mkCred("a"))
	p.Add(mkCred("b"))

	p.Disable("a", "dead")
	p.Cooldown("b", CoolSoft, time.Hour, "rate limited")
	if got := p.Pick(); got != nil {
		t.Fatalf("Pick with no healthy account = %v, want nil", got)
	}

	p.Enable("a")
	if got := p.Pick(); got == nil || got.UID != "a" {
		t.Fatalf("after enable, Pick = %v, want a", got)
	}
}

func TestPickPrefersHealthyOverErroring(t *testing.T) {
	p := New("")
	p.Add(mkCred("a"))
	p.Add(mkCred("b"))

	p.NoteError("a", 100, time.Hour)
	if got := p.Pick(); got == nil || got.UID != "b" {
		t.Fatalf("Pick should prefer the account without errors, got %v", got)
	}
}

func TestNoteErrorThresholdCools(t *testing.T) {
	p := New("")
	p.Add(mkCred("a"))

	p.NoteError("a", 2, time.Hour)
	if st, _ := p.Status("a"); st.Cooling {
		t.Fatal("should not cool before threshold")
	}
	p.NoteError("a", 2, time.Hour)
	st, _ := p.Status("a")
	if !st.Cooling {
		t.Fatal("should cool after reaching threshold")
	}
	if st.Reason != "consecutive errors" {
		t.Fatalf("reason = %q", st.Reason)
	}
}

func TestNoteSuccessResetsErrors(t *testing.T) {
	p := New("")
	p.Add(mkCred("a"))
	p.NoteError("a", 5, time.Hour)
	p.NoteSuccess("a")
	st, _ := p.Status("a")
	if st.ErrCount != 0 {
		t.Fatalf("err_count = %d, want 0", st.ErrCount)
	}
	if st.LastOK.IsZero() {
		t.Fatal("last_ok should be recorded")
	}
}

func TestSyncToDir(t *testing.T) {
	p := New("")
	p.Add(mkCred("a"))
	p.Add(mkCred("b"))
	p.NoteError("a", 100, time.Hour) // 状态应当保留

	p.SyncToDir([]*cred.Cred{mkCred("a"), mkCred("c")})

	if _, ok := p.Status("b"); ok {
		t.Fatal("b should have been removed")
	}
	if _, ok := p.Status("c"); !ok {
		t.Fatal("c should have been added")
	}
	if st, _ := p.Status("a"); st.ErrCount != 1 {
		t.Fatalf("a err_count = %d, want 1 (status preserved)", st.ErrCount)
	}
}

func TestCount(t *testing.T) {
	p := New("")
	p.Add(mkCred("a"))
	p.Add(mkCred("b"))
	p.Disable("b", "dead")
	total, healthy := p.Count()
	if total != 2 || healthy != 1 {
		t.Fatalf("Count = (%d,%d), want (2,1)", total, healthy)
	}
}

func TestRemove(t *testing.T) {
	p := New("")
	p.Add(mkCred("a"))
	if !p.Remove("a") {
		t.Fatal("Remove should return true")
	}
	if p.Remove("a") {
		t.Fatal("Remove on missing account should return false")
	}
	if _, ok := p.Status("a"); ok {
		t.Fatal("account still present")
	}
}
