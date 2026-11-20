package mail

import (
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/mailcatch/mailstore"
	"github.com/jtarchie/mailcatch/mailstore/storetest"

	"github.com/jtarchie/topbanana/internal/store"
	tbstoretest "github.com/jtarchie/topbanana/internal/storetest"
)

func open(t *testing.T, cfg storetest.Config) *Store {
	t.Helper()
	// A per-test tree, so a shared Minio bucket never lets one test's Reap count another's mail.
	prefix := store.MailPrefix + strings.ToLower(strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())) + "/"
	s := New(tbstoretest.New(t, 0), Options{Now: cfg.Now, TTL: cfg.TTL, MaxPerInbox: cfg.MaxPerInbox, Prefix: prefix})
	t.Cleanup(func() {
		_, _ = s.Reap(t.Context())
	})
	return s
}

func TestConformance(t *testing.T) {
	t.Parallel()
	storetest.Run(t, func(t *testing.T, cfg storetest.Config) mailstore.Store {
		t.Helper()
		return open(t, cfg)
	})
}

func TestTenantBudgetEvictsOldestAndLongHeadersSurvive(t *testing.T) {
	t.Parallel()
	c := storetest.NewClock()
	s := open(t, storetest.Config{Now: c.Now})
	s.opts.MaxPerTenant = 100

	long := strings.Repeat("ü", 200) // multi-byte, past listingField: must truncate on a rune boundary
	for range 4 {
		m := storetest.Msg(c, "site", "x", long)
		m.Raw = []byte(strings.Repeat("y", 40))
		storetest.Deliver(t, s, m)
		c.Advance(time.Millisecond)
	}
	_, err := s.Reap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List(t.Context(), "site", "", 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("budget 100B over 40B messages keeps newest 2, got %d (%v)", len(list), err)
	}
	if !strings.HasPrefix(long, list[0].Subject) || list[0].Subject == "" {
		t.Fatalf("subject not a valid truncation: %q", list[0].Subject)
	}
	got, err := s.Get(t.Context(), "site", "x", list[0].ID)
	if err != nil || string(got.Raw) != strings.Repeat("y", 40) {
		t.Fatalf("get = %+v, %v", got, err)
	}
}

func TestBadTenantAndID(t *testing.T) {
	t.Parallel()
	c := storetest.NewClock()
	s := open(t, storetest.Config{Now: c.Now})
	for _, tenant := range []string{"", "../x", "_acme", "a/b"} {
		err := s.Deliver(t.Context(), []mailstore.Message{storetest.Msg(c, tenant, "x", "s")})
		if err == nil {
			t.Errorf("tenant %q accepted", tenant)
		}
	}
	storetest.Deliver(t, s, storetest.Msg(c, "site", "x", "s"))
	list, _ := s.List(t.Context(), "site", "x", 1)
	// The id is only a key prefix; a truncated id must not match by prefix.
	_, err := s.Get(t.Context(), "site", "x", list[0].ID[:len(list[0].ID)-1])
	if err == nil {
		t.Fatal("truncated id resolved")
	}
	_, err = s.Get(t.Context(), "other", "x", list[0].ID)
	if err == nil {
		t.Fatal("id resolved across tenants")
	}
}
