package mail

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	unknownSender   = "(inconnu)"
	untitledSubject = "(sans objet)"
)

func (c *Collector) query(cl *imapclient.Client, fresh bool) (Data, error) {
	if fresh {
		if err := cl.WaitGreeting(); err != nil {
			return Data{}, fmt.Errorf("greeting: %w", err)
		}
		password := strings.ReplaceAll(c.cfg.AppPassword, " ", "")
		if err := cl.Login(c.cfg.User, password).Wait(); err != nil {
			return Data{}, fmt.Errorf("login: %w", err)
		}
	}
	if _, err := cl.Select(c.cfg.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return Data{}, fmt.Errorf("examine %s: %w", c.cfg.Mailbox, err)
	}
	found, err := cl.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		return Data{}, fmt.Errorf("search unseen: %w", err)
	}
	set, _ := found.All.(imap.UIDSet)
	uids, ok := set.Nums()
	if !ok {
		return Data{}, errors.New("search unseen: open-ended UID range")
	}
	data := Data{Account: c.cfg.User, Unseen: len(uids), Items: []Item{}}
	if len(uids) == 0 || c.cfg.Latest <= 0 {
		return data, nil
	}
	slices.Sort(uids)
	latest := imap.UIDSetNum(uids[max(len(uids)-c.cfg.Latest, 0):]...)
	msgs, err := cl.Fetch(latest, &imap.FetchOptions{UID: true, Envelope: true, InternalDate: true}).Collect()
	if err != nil {
		return Data{}, fmt.Errorf("fetch envelopes: %w", err)
	}
	slices.SortFunc(msgs, newestFirst)
	for _, m := range msgs {
		data.Items = append(data.Items, item(m))
	}
	return data, nil
}

func newestFirst(a, b *imapclient.FetchMessageBuffer) int {
	return cmp.Or(b.InternalDate.Compare(a.InternalDate), cmp.Compare(b.UID, a.UID))
}

func item(m *imapclient.FetchMessageBuffer) Item {
	env := cmp.Or(m.Envelope, &imap.Envelope{})
	return Item{
		From:    cmp.Or(sender(env.From), unknownSender),
		Subject: cmp.Or(clean(env.Subject), untitledSubject),
		At:      m.InternalDate,
	}
}

func sender(addrs []imap.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	return cmp.Or(clean(addrs[0].Name), addrs[0].Addr())
}

func clean(s string) string { return strings.Join(strings.Fields(s), " ") }
