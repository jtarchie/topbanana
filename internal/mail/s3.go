// Package mail is Top Banana's mailstore driver: caught mail lives in the site bucket under _mail/{slug}/.
package mail

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jtarchie/mailcatch/mailstore"

	"github.com/jtarchie/topbanana/internal/store"
)

var _ mailstore.Store = (*Store)(nil)

// Options bound the driver. Zero values take the defaults below.
type Options struct {
	TTL          time.Duration // used when a message arrives without its own ExpiresAt
	MaxPerInbox  int
	MaxPerTenant int64  // bytes; the reaper evicts a site's oldest mail past it
	MaxTotal     int64  // bytes across every site; SMTP defers (452) while over
	Prefix       string // bucket root, store.MailPrefix unless a test needs its own tree
	Now          func() time.Time
}

func (o Options) withDefaults() Options {
	if o.TTL <= 0 {
		o.TTL = 7 * 24 * time.Hour
	}
	if o.MaxPerInbox <= 0 {
		o.MaxPerInbox = 500
	}
	if o.MaxPerTenant <= 0 {
		o.MaxPerTenant = 50 << 20
	}
	if o.MaxTotal <= 0 {
		o.MaxTotal = 5 << 30
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Prefix == "" {
		o.Prefix = store.MailPrefix
	}
	return o
}

// Store keys every listed field into _mail/{slug}/{rev}~{rand}~{expMs}~{inbox}~{domain}~{b64 from}~{b64 subj} so a page is one LIST, never a HEAD per row; rev inverts time for newest-first, and the id {rev}~{rand} is a key prefix so Get needs no index.
type Store struct {
	s    *store.Store
	opts Options
	over atomic.Bool
}

// New wraps the site bucket. Call RunReaper, or expired mail stays on disk (reads already hide it).
func New(s *store.Store, opts Options) *Store {
	return &Store{s: s, opts: opts.withDefaults()}
}

const (
	sep = "~"
	// listingField keeps keys under S3's 1024-byte limit; a listing only needs enough subject to recognise it.
	listingField = 120
)

// ErrBadTenant guards the key concatenation: a tenant is a slug, never a path.
var ErrBadTenant = errors.New("mail: invalid tenant")

func (s *Store) tenantPrefix(tenant string) (string, error) {
	if tenant == "" || strings.ContainsAny(tenant, "/~.") || strings.HasPrefix(tenant, "_") {
		return "", ErrBadTenant
	}
	return s.opts.Prefix + tenant + "/", nil
}

type entry struct {
	key  string
	size int64
	msg  mailstore.Message
}

func (s *Store) Deliver(ctx context.Context, msgs []mailstore.Message) error {
	for _, m := range msgs {
		prefix, err := s.tenantPrefix(m.Tenant)
		if err != nil {
			return err
		}
		expires := m.ExpiresAt
		if expires.IsZero() {
			expires = m.ReceivedAt.Add(s.opts.TTL)
		}
		key := prefix + strings.Join([]string{
			fmt.Sprintf("%019d", math.MaxInt64-m.ReceivedAt.UnixNano()),
			strings.ToLower(rand.Text()[:10]),
			strconv.FormatInt(expires.UnixMilli(), 10),
			m.Inbox,
			m.Domain,
			encodeField(m.From),
			encodeField(m.Subject),
		}, sep)
		// One synchronous PUT per copy: SMTP acks on our return, so return means durable.
		err = s.s.WriteRaw(ctx, key, string(m.Raw), "message/rfc822", map[string]string{"from": asciiOnly(m.From)})
		if err != nil {
			return fmt.Errorf("deliver %s/%s: %w", m.Tenant, m.Inbox, err)
		}
	}
	return nil
}

func encodeField(s string) string {
	if len(s) > listingField {
		s = s[:listingField]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func asciiOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, s)
}

// parseKey refuses malformed keys so a stray object under _mail/ is skipped, not rendered as garbage.
func (s *Store) parseKey(key string) (mailstore.Message, bool) {
	tenant, rest, ok := strings.Cut(strings.TrimPrefix(key, s.opts.Prefix), "/")
	if !ok {
		return mailstore.Message{}, false
	}
	f := strings.Split(rest, sep)
	if len(f) != 7 {
		return mailstore.Message{}, false
	}
	rev, err1 := strconv.ParseInt(f[0], 10, 64)
	exp, err2 := strconv.ParseInt(f[2], 10, 64)
	from, err3 := base64.RawURLEncoding.DecodeString(f[5])
	subject, err4 := base64.RawURLEncoding.DecodeString(f[6])
	err := errors.Join(err1, err2, err3, err4)
	if err != nil {
		return mailstore.Message{}, false
	}
	return mailstore.Message{
		ID:         f[0] + sep + f[1],
		Tenant:     tenant,
		Inbox:      f[3],
		Domain:     f[4],
		ReceivedAt: time.Unix(0, math.MaxInt64-rev).UTC(),
		ExpiresAt:  time.UnixMilli(exp).UTC(),
		From:       string(from),
		Subject:    string(subject),
	}, true
}

// entries lists a prefix newest first, parsed; expired ones are included so the reaper can see them.
func (s *Store) entries(ctx context.Context, prefix string) ([]entry, error) {
	objs, err := s.s.ListPrefixInfo(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("list mail %s: %w", prefix, err)
	}
	out := make([]entry, 0, len(objs))
	for _, o := range objs {
		m, ok := s.parseKey(o.Key)
		if !ok {
			continue
		}
		m.Size = int(o.Size)
		out = append(out, entry{key: o.Key, size: o.Size, msg: m})
	}
	// The memory backend lists in map order; S3 lists lexically. Sort so both are newest first.
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.key, b.key) })
	return out, nil
}

func (s *Store) live(m mailstore.Message) bool { return m.ExpiresAt.After(s.opts.Now()) }

func (s *Store) List(ctx context.Context, tenant, inbox string, limit int) ([]mailstore.Message, error) {
	prefix, err := s.tenantPrefix(tenant)
	if err != nil {
		return nil, err
	}
	es, err := s.entries(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]mailstore.Message, 0, min(limit, len(es)))
	for _, e := range es {
		if len(out) == limit {
			break
		}
		if s.live(e.msg) && (inbox == "" || e.msg.Inbox == inbox) {
			out = append(out, e.msg)
		}
	}
	return out, nil
}

// find resolves an id within tenant and inbox; any mismatch, malformed id, or expiry is ErrNotFound.
func (s *Store) find(ctx context.Context, tenant, inbox, id string) (entry, error) {
	prefix, err := s.tenantPrefix(tenant)
	if err != nil {
		return entry{}, mailstore.ErrNotFound
	}
	rev, rnd, ok := strings.Cut(id, sep)
	if !ok || len(rev) != 19 || rnd == "" || strings.ContainsAny(id, "/.") {
		return entry{}, mailstore.ErrNotFound
	}
	es, err := s.entries(ctx, prefix+id+sep)
	if err != nil {
		return entry{}, err
	}
	for _, e := range es {
		if e.msg.ID == id && e.msg.Inbox == inbox && s.live(e.msg) {
			return e, nil
		}
	}
	return entry{}, mailstore.ErrNotFound
}

func (s *Store) Get(ctx context.Context, tenant, inbox, id string) (mailstore.Message, error) {
	e, err := s.find(ctx, tenant, inbox, id)
	if err != nil {
		return mailstore.Message{}, err
	}
	obj, err := s.s.ReadRaw(ctx, e.key)
	if err != nil {
		return mailstore.Message{}, fmt.Errorf("read mail %s: %w", id, err)
	}
	if obj.Content == "" && e.size > 0 {
		return mailstore.Message{}, mailstore.ErrNotFound // reaped between LIST and GET
	}
	m := e.msg
	m.Raw = []byte(obj.Content)
	m.Size = len(m.Raw)
	return m, nil
}

func (s *Store) Delete(ctx context.Context, tenant, inbox, id string) error {
	e, err := s.find(ctx, tenant, inbox, id)
	if err != nil {
		return err
	}
	err = s.s.DeleteRaw(ctx, e.key)
	if err != nil {
		return fmt.Errorf("delete mail %s: %w", id, err)
	}
	return nil
}

// DeleteTenant removes every message a site holds: the delete-site cascade and "empty inbox".
func (s *Store) DeleteTenant(ctx context.Context, tenant string) (int, error) {
	prefix, err := s.tenantPrefix(tenant)
	if err != nil {
		return 0, err
	}
	keys, err := s.s.ListPrefix(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("list mail %s: %w", tenant, err)
	}
	for i, k := range keys {
		err := s.s.DeleteRaw(ctx, k)
		if err != nil {
			return i, fmt.Errorf("delete mail %s: %w", k, err)
		}
	}
	return len(keys), nil
}

func (s *Store) OverCapacity() bool { return s.over.Load() }

// Reap also recomputes the global total OverCapacity reports, so a full bucket defers mail until a sweep frees room.
func (s *Store) Reap(ctx context.Context) (int64, error) {
	es, err := s.entries(ctx, s.opts.Prefix)
	if err != nil {
		return 0, err
	}
	type box struct{ tenant, inbox string }
	perInbox := map[box]int{}
	perTenant := map[string]int64{}
	var total, removed int64
	// Newest first, so every count below is "how many newer ones survived".
	for _, e := range es {
		b := box{e.msg.Tenant, e.msg.Inbox}
		drop := !s.live(e.msg) || perInbox[b] >= s.opts.MaxPerInbox || perTenant[b.tenant]+e.size > s.opts.MaxPerTenant
		if !drop {
			perInbox[b]++
			perTenant[b.tenant] += e.size
			total += e.size
			continue
		}
		err := s.s.DeleteRaw(ctx, e.key)
		if err != nil {
			return removed, fmt.Errorf("reap %s: %w", e.key, err)
		}
		removed++
	}
	s.over.Store(total > s.opts.MaxTotal)
	return removed, nil
}

// RunReaper exists to free space; reads already hide expired mail the instant it expires.
func (s *Store) RunReaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.Reap(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Error("mail.reap", "err", err)
			} else if n > 0 {
				slog.Info("mail.reaped", "messages", n)
			}
		}
	}
}
