package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

var (
	_ collector.Collector  = (*Collector)(nil)
	_ collector.Summarizer = (*Collector)(nil)
	_ collector.Timeouter  = (*Collector)(nil)

	paris = time.FixedZone("Europe/Paris", 2*3600)
)

const (
	testUser     = "benjamin@kolapsis.com"
	testPassword = "wxyzwxyzwxyzwxyz"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type listener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *listener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, conn)
		l.mu.Unlock()
	}
	return conn, err
}

func (l *listener) dropAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.conns {
		_ = conn.Close()
	}
	l.conns = nil
}

type harness struct {
	t       *testing.T
	user    *imapmemserver.User
	ln      *listener
	c       *Collector
	dials   atomic.Int32
	dialErr error
	wire    *syncBuffer
	logs    *syncBuffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(testUser, testPassword)
	require.NoError(t, user.Create("INBOX", nil))
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		InsecureAuth: true,
		Logger:       log.New(io.Discard, "", 0),
	})
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ln := &listener{Listener: raw}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	h := &harness{t: t, user: user, ln: ln, wire: &syncBuffer{}, logs: &syncBuffer{}}
	h.c = New(config.Mail{
		Host:        raw.Addr().String(),
		User:        testUser,
		AppPassword: testPassword,
		Mailbox:     "INBOX",
		Latest:      3,
	}, collector.Deps{
		Clock: clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, paris)),
		Hist:  store.NewMem(),
		Log:   slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Loc:   paris,
	})
	h.c.dial = h.dial
	t.Cleanup(func() {
		h.c.mu.Lock()
		defer h.c.mu.Unlock()
		h.c.drop()
	})
	return h
}

func (h *harness) dial(ctx context.Context) (*imapclient.Client, error) {
	h.dials.Add(1)
	if h.dialErr != nil {
		return nil, h.dialErr
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", h.ln.Addr().String())
	if err != nil {
		return nil, err
	}
	opts := h.c.options()
	opts.DebugWriter = h.wire
	return imapclient.New(conn, opts), nil
}

func rawMessage(from, subject string, date time.Time) string {
	headers := []string{
		"From: " + from,
		"To: " + testUser,
		"Date: " + date.Format(time.RFC1123Z),
		"Message-ID: <" + strconv.FormatInt(date.Unix(), 10) + "@example.org>",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	}
	if subject != "" {
		headers = append(headers, "Subject: "+subject)
	}
	return strings.Join(headers, "\r\n") + "\r\n\r\ncorps du message\r\n"
}

func (h *harness) add(from, subject string, at time.Time, flags ...imap.Flag) imap.UID {
	h.t.Helper()
	data, err := h.user.Append("INBOX", strings.NewReader(rawMessage(from, subject, at)), &imap.AppendOptions{Time: at, Flags: flags})
	require.NoError(h.t, err)
	return data.UID
}

func (h *harness) markSeen(uid imap.UID) {
	h.t.Helper()
	conn, err := net.Dial("tcp", h.ln.Addr().String())
	require.NoError(h.t, err)
	other := imapclient.New(conn, nil)
	defer func() { _ = other.Close() }()
	require.NoError(h.t, other.Login(testUser, testPassword).Wait())
	_, err = other.Select("INBOX", nil).Wait()
	require.NoError(h.t, err)
	flags := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}
	require.NoError(h.t, other.Store(imap.UIDSetNum(uid), flags, nil).Close())
}

func (h *harness) collect() Data {
	h.t.Helper()
	got, err := h.c.Collect(context.Background())
	require.NoError(h.t, err)
	return got.(Data)
}

func (h *harness) seed() {
	base := time.Date(2026, 10, 7, 8, 0, 0, 0, paris)
	h.add("Archives <archives@example.org>", "Déjà lu", base, imap.FlagSeen)
	h.add(mime.QEncoding.Encode("utf-8", "Hélène Dupont")+" <helene@example.org>", "=?UTF-8?Q?R=C3=A9union?=", base.Add(time.Hour))
	h.add("<noreply@github.com>", "[kolapsis/maintenant] Issue #312", base.Add(90*time.Minute))
	h.add("Qonto <notifications@qonto.example>", "", base.Add(100*time.Minute))
	h.add("Stripe <notifications@stripe.com>", mime.BEncoding.Encode("utf-8", "Paiement reçu : 29,00 € – Maintenant Pro"), base.Add(30*time.Minute))
	h.add("Cap Digital <contact@capdigital.example>", "Comité de suivi", base.Add(2*time.Hour))
	h.add("Archives <archives@example.org>", "Autre message lu", base.Add(3*time.Hour), imap.FlagSeen)
}

func TestCollect(t *testing.T) {
	h := newHarness(t)
	h.seed()

	got := h.collect()

	assert.Equal(t, Data{
		Account: testUser,
		Unseen:  5,
		Items: []Item{
			{From: "Cap Digital", Subject: "Comité de suivi", At: time.Date(2026, 10, 7, 10, 0, 0, 0, paris)},
			{From: "Qonto", Subject: untitledSubject, At: time.Date(2026, 10, 7, 9, 40, 0, 0, paris)},
			{From: "Stripe", Subject: "Paiement reçu : 29,00 € – Maintenant Pro", At: time.Date(2026, 10, 7, 8, 30, 0, 0, paris)},
		},
	}, normalize(got))
}

func normalize(d Data) Data {
	for i := range d.Items {
		d.Items[i].At = d.Items[i].At.In(paris)
	}
	return d
}

func TestCollectDecodesSubjectsAndSenders(t *testing.T) {
	h := newHarness(t)
	h.c.cfg.Latest = 10
	h.seed()

	got := h.collect()

	byFrom := map[string]string{}
	for _, it := range got.Items {
		byFrom[it.From] = it.Subject
	}
	assert.Equal(t, "Réunion", byFrom["Hélène Dupont"])
	assert.Equal(t, "[kolapsis/maintenant] Issue #312", byFrom["noreply@github.com"])
	assert.Equal(t, "Paiement reçu : 29,00 € – Maintenant Pro", byFrom["Stripe"])
}

func TestCollectOrdersByDateNotByUID(t *testing.T) {
	h := newHarness(t)
	base := time.Date(2026, 10, 7, 8, 0, 0, 0, paris)
	h.add("A <a@example.org>", "first uid, newest", base.Add(3*time.Hour))
	h.add("B <b@example.org>", "second uid, oldest", base)
	h.add("C <c@example.org>", "third uid, middle", base.Add(time.Hour))

	got := h.collect()

	assert.Equal(t, []string{"first uid, newest", "third uid, middle", "second uid, oldest"}, subjects(got))
}

func subjects(d Data) []string {
	out := make([]string, 0, len(d.Items))
	for _, it := range d.Items {
		out = append(out, it.Subject)
	}
	return out
}

func TestCollectKeepsTheHighestUIDs(t *testing.T) {
	h := newHarness(t)
	base := time.Date(2026, 10, 7, 8, 0, 0, 0, paris)
	for i := range 6 {
		h.add("A <a@example.org>", "message "+strconv.Itoa(i+1), base.Add(time.Duration(i)*time.Minute))
	}

	got := h.collect()

	assert.Equal(t, 6, got.Unseen)
	assert.Equal(t, []string{"message 6", "message 5", "message 4"}, subjects(got))
}

func TestCollectLatestLimits(t *testing.T) {
	tests := []struct {
		name      string
		latest    int
		wantItems int
	}{
		{"more than available", 10, 2},
		{"exact", 2, 2},
		{"none requested", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.c.cfg.Latest = tt.latest
			h.add("A <a@example.org>", "one", time.Date(2026, 10, 7, 8, 0, 0, 0, paris))
			h.add("A <a@example.org>", "two", time.Date(2026, 10, 7, 9, 0, 0, 0, paris))

			got := h.collect()

			assert.Equal(t, 2, got.Unseen)
			assert.Len(t, got.Items, tt.wantItems)
		})
	}
}

func TestCollectNothingUnseen(t *testing.T) {
	h := newHarness(t)
	h.add("A <a@example.org>", "lu", time.Date(2026, 10, 7, 8, 0, 0, 0, paris), imap.FlagSeen)

	got := h.collect()

	assert.Equal(t, 0, got.Unseen)
	assert.Equal(t, testUser, got.Account)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"account": "benjamin@kolapsis.com", "unseen": 0, "items": []}`, string(b))
}

func TestCollectIsReadOnly(t *testing.T) {
	h := newHarness(t)
	h.seed()

	h.collect()

	wire := h.wire.String()
	assert.Contains(t, wire, "EXAMINE")
	assert.Contains(t, wire, "[READ-ONLY]")
	assert.NotContains(t, wire, " SELECT ")
	assert.NotContains(t, wire, " STORE ")
	assert.NotContains(t, wire, "BODY[")
	assert.NotContains(t, wire, "RFC822")
	status, err := h.user.Status("INBOX", &imap.StatusOptions{NumUnseen: true})
	require.NoError(t, err)
	assert.EqualValues(t, 5, *status.NumUnseen)
}

func TestCollectReusesTheConnection(t *testing.T) {
	h := newHarness(t)
	base := time.Date(2026, 10, 7, 8, 0, 0, 0, paris)
	first := h.add("A <a@example.org>", "first", base)
	h.add("A <a@example.org>", "second", base.Add(time.Minute))

	assert.Equal(t, 2, h.collect().Unseen)

	h.add("A <a@example.org>", "third", base.Add(2*time.Minute))
	h.markSeen(first)
	got := h.collect()

	assert.EqualValues(t, 1, h.dials.Load())
	assert.Equal(t, 2, got.Unseen)
	assert.Equal(t, []string{"third", "second"}, subjects(got))
}

func TestCollectReconnectsAfterDroppedConnection(t *testing.T) {
	h := newHarness(t)
	h.seed()
	require.Equal(t, 5, h.collect().Unseen)

	h.ln.dropAll()
	h.add("New <new@example.org>", "arrivé pendant la coupure", time.Date(2026, 10, 7, 11, 0, 0, 0, paris))
	got := h.collect()

	assert.EqualValues(t, 2, h.dials.Load())
	assert.Equal(t, 6, got.Unseen)
	assert.Equal(t, "arrivé pendant la coupure", got.Items[0].Subject)
	assert.Contains(t, h.logs.String(), "reconnecting")
}

func TestCollectRecoversAfterFailedDial(t *testing.T) {
	h := newHarness(t)
	h.seed()
	h.dialErr = errors.New("network is unreachable")

	got, err := h.c.Collect(context.Background())

	require.ErrorContains(t, err, "connect to "+h.c.cfg.Host)
	require.ErrorContains(t, err, "network is unreachable")
	assert.Nil(t, got)
	h.dialErr = nil
	assert.Equal(t, 5, h.collect().Unseen)
	assert.EqualValues(t, 2, h.dials.Load())
}

func TestCollectLoginFailure(t *testing.T) {
	h := newHarness(t)
	h.seed()
	h.c.cfg.AppPassword = "not-the-password"

	got, err := h.c.Collect(context.Background())

	require.ErrorContains(t, err, "login")
	assert.NotContains(t, err.Error(), "not-the-password")
	assert.NotContains(t, h.logs.String(), "not-the-password")
	assert.Nil(t, got)
	assert.EqualValues(t, 1, h.dials.Load())

	h.c.cfg.AppPassword = testPassword
	assert.Equal(t, 5, h.collect().Unseen)
	assert.EqualValues(t, 2, h.dials.Load())
}

func TestCollectIgnoresSpacesInTheAppPassword(t *testing.T) {
	h := newHarness(t)
	h.seed()
	h.c.cfg.AppPassword = "wxyz wxyz wxyz wxyz"

	assert.Equal(t, 5, h.collect().Unseen)
}

func TestCollectDoesNotRetryServerReplies(t *testing.T) {
	h := newHarness(t)
	h.seed()
	require.Equal(t, 5, h.collect().Unseen)
	require.NoError(t, h.user.Delete("INBOX"))

	got, err := h.c.Collect(context.Background())

	require.ErrorContains(t, err, "examine INBOX")
	assert.Nil(t, got)
	assert.EqualValues(t, 1, h.dials.Load())

	require.NoError(t, h.user.Create("INBOX", nil))
	h.add("A <a@example.org>", "retour", time.Date(2026, 10, 7, 8, 0, 0, 0, paris))
	assert.Equal(t, 1, h.collect().Unseen)
	assert.EqualValues(t, 2, h.dials.Load())
}

func TestCollectReadsTheConfiguredMailbox(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.user.Create("Clients", nil))
	_, err := h.user.Append("Clients", strings.NewReader(rawMessage("A <a@example.org>", "client", time.Date(2026, 10, 7, 8, 0, 0, 0, paris))), &imap.AppendOptions{})
	require.NoError(t, err)
	h.add("A <a@example.org>", "inbox", time.Date(2026, 10, 7, 8, 0, 0, 0, paris))
	h.c.cfg.Mailbox = "Clients"

	got := h.collect()

	assert.Equal(t, []string{"client"}, subjects(got))
}

func TestCollectStopsAtTheDeadline(t *testing.T) {
	h := newHarness(t)
	silent, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	h.c.dial = func(context.Context) (*imapclient.Client, error) { return imapclient.New(silent, nil), nil }
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	got, err := h.c.Collect(ctx)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, got)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Nil(t, h.c.client)
}

func TestCollectCanceledContext(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.c.Collect(ctx)

	require.ErrorIs(t, err, context.Canceled)
}

func TestCollectDecodesLegacyCharsets(t *testing.T) {
	addr := scriptedServer(t, `* 1 FETCH (UID 41 INTERNALDATE "07-Oct-2026 09:15:00 +0200" ENVELOPE ("Wed, 07 Oct 2026 09:15:00 +0200" "=?windows-1252?Q?Pr=E9visionnel_=96_V3?=" (("=?iso-8859-15?Q?H=E9l=E8ne_Dupont?=" NIL "helene" "example.org")) NIL NIL NIL NIL NIL NIL "<41@example.org>"))`+"\r\n"+
		`* 2 FETCH (UID 42 INTERNALDATE "07-Oct-2026 10:30:00 +0200" ENVELOPE ("Wed, 07 Oct 2026 10:30:00 +0200" "=?iso-8859-15?Q?Virement_de_447_=A4?=" ((NIL NIL "notifications" "qonto.example")) NIL NIL NIL NIL NIL NIL "<42@example.org>"))`+"\r\n")
	h := newHarness(t)
	h.c.cfg.Host = addr
	h.c.dial = func(ctx context.Context) (*imapclient.Client, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		return imapclient.New(conn, h.c.options()), nil
	}

	got := h.collect()

	assert.Equal(t, 2, got.Unseen)
	require.Len(t, got.Items, 2)
	assert.Equal(t, Item{From: "notifications@qonto.example", Subject: "Virement de 447 €"}, withoutDate(got.Items[0]))
	assert.Equal(t, Item{From: "Hélène Dupont", Subject: "Prévisionnel – V3"}, withoutDate(got.Items[1]))
	assert.True(t, time.Date(2026, 10, 7, 10, 30, 0, 0, paris).Equal(got.Items[0].At))
}

func withoutDate(it Item) Item {
	it.At = time.Time{}
	return it
}

func scriptedServer(t *testing.T, fetched string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "* OK scripted server ready\r\n")
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tag, cmd, _ := strings.Cut(strings.TrimSpace(line), " ")
			var reply string
			switch {
			case strings.HasPrefix(cmd, "CAPABILITY"):
				reply = "* CAPABILITY IMAP4rev1\r\n" + tag + " OK done\r\n"
			case strings.HasPrefix(cmd, "EXAMINE"):
				reply = "* 2 EXISTS\r\n" + tag + " OK [READ-ONLY] done\r\n"
			case strings.HasPrefix(cmd, "UID SEARCH"):
				reply = "* SEARCH 41 42\r\n" + tag + " OK done\r\n"
			case strings.HasPrefix(cmd, "UID FETCH"):
				reply = fetched + tag + " OK done\r\n"
			default:
				reply = tag + " OK done\r\n"
			}
			if _, err := io.WriteString(conn, reply); err != nil {
				return
			}
		}
	}()
	return ln.Addr().String()
}

func TestDialTLSVerifiesCertificates(t *testing.T) {
	selfSigned := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(selfSigned.Close)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: selfSigned.TLS.Certificates})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		if conn, err := ln.Accept(); err == nil {
			_ = conn.(*tls.Conn).Handshake()
			_ = conn.Close()
		}
	}()
	c := New(config.Mail{Host: ln.Addr().String()}, collector.Deps{})

	_, err = c.dialTLS(context.Background())

	var invalid *tls.CertificateVerificationError
	require.ErrorAs(t, err, &invalid)
}

func TestSummary(t *testing.T) {
	c := New(config.Mail{}, collector.Deps{})
	items := []Item{{From: "Stripe", Subject: "Paiement reçu"}, {From: "GitHub", Subject: "Issue #312"}}
	tests := []struct {
		name string
		data any
		want string
	}{
		{"several unseen", Data{Unseen: 7, Items: items}, "7 non-lus · dernier : Stripe – Paiement reçu"},
		{"one unseen", Data{Unseen: 1, Items: items[:1]}, "1 non-lu · dernier : Stripe – Paiement reçu"},
		{"none unseen", Data{Items: []Item{}}, "0 non-lu"},
		{"foreign data", "not mail data", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, c.Summary(tt.data))
		})
	}
}

func TestIdentity(t *testing.T) {
	c := New(config.Mail{Interval: time.Minute}, collector.Deps{})

	assert.Equal(t, "mail", c.Name())
	assert.Equal(t, time.Minute, c.Interval())
	assert.Equal(t, 40*time.Second, c.Timeout())
}
