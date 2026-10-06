package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
)

const (
	collectTimeout = 40 * time.Second
	dialTimeout    = 15 * time.Second
)

type dialFunc func(ctx context.Context) (*imapclient.Client, error)

type Collector struct {
	cfg  config.Mail
	deps collector.Deps
	dial dialFunc

	mu     sync.Mutex
	client *imapclient.Client
}

func New(cfg config.Mail, deps collector.Deps) *Collector {
	c := &Collector{cfg: cfg, deps: deps}
	c.dial = c.dialTLS
	return c
}

func (c *Collector) Name() string { return "mail" }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

func (c *Collector) Timeout() time.Duration { return collectTimeout }

func (c *Collector) Collect(ctx context.Context) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reused := c.client != nil
	data, err := c.run(ctx)
	if err != nil && reused && ctx.Err() == nil && !isReply(err) {
		c.deps.Log.Debug("mail connection was stale, reconnecting", "err", err)
		data, err = c.run(ctx)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Collector) run(ctx context.Context) (Data, error) {
	fresh := c.client == nil
	if fresh {
		cl, err := c.dial(ctx)
		if err != nil {
			return Data{}, fmt.Errorf("connect to %s: %w", c.cfg.Host, err)
		}
		c.client = cl
	}
	cl := c.client
	stop := context.AfterFunc(ctx, func() { _ = cl.Close() })
	defer stop()
	data, err := c.query(cl, fresh)
	if err != nil {
		c.drop()
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", ctx.Err(), err)
		}
		return Data{}, err
	}
	return data, nil
}

func (c *Collector) drop() {
	if c.client != nil {
		_ = c.client.Close()
		c.client = nil
	}
}

func (c *Collector) dialTLS(ctx context.Context) (*imapclient.Client, error) {
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout},
		Config:    &tls.Config{NextProtos: []string{"imap"}},
	}
	conn, err := d.DialContext(ctx, "tcp", c.cfg.Host)
	if err != nil {
		return nil, err
	}
	return imapclient.New(conn, c.options()), nil
}

func (c *Collector) options() *imapclient.Options {
	return &imapclient.Options{WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}}
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	out := fmt.Sprintf("%d non-lu", d.Unseen)
	if d.Unseen > 1 {
		out += "s"
	}
	if len(d.Items) > 0 {
		out += fmt.Sprintf(" · dernier : %s – %s", d.Items[0].From, d.Items[0].Subject)
	}
	return out
}

func isReply(err error) bool {
	var reply *imap.Error
	return errors.As(err, &reply)
}
