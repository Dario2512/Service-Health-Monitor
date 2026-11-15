// Package alert tells people when the monitor changes its mind about a
// target: once when a target is confirmed down, and once when it is
// confirmed back up. Each message goes to every configured channel; a channel
// that fails does not stop the others.
package alert

import (
	"context"
	"fmt"
	"sync"
	"time"

	"atad-project/internal/monitor"
)

// Message is what a channel delivers.
type Message struct {
	Kind       string             `json:"kind"` // "down" or "recovered"
	Subject    string             `json:"subject"`
	Text       string             `json:"text"`
	Transition monitor.Transition `json:"transition"`
}

// Channel is one way of reaching a person.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// Delivery records one attempt to send one message on one channel, so the
// operator can see that an alert went out and where.
type Delivery struct {
	ID         int       `json:"id"`
	At         time.Time `json:"at"`
	Channel    string    `json:"channel"`
	Kind       string    `json:"kind"`
	TargetID   int       `json:"target_id"`
	TargetName string    `json:"target_name"`
	Subject    string    `json:"subject"`
	Delivered  bool      `json:"delivered"`
	Error      string    `json:"error,omitempty"`
}

const maxDeliveries = 500

// Dispatcher fans transitions out to channels without blocking the checker.
type Dispatcher struct {
	channels []Channel
	timeout  time.Duration

	mu         sync.Mutex
	deliveries []Delivery
	nextID     int
	epoch      int
	wg         sync.WaitGroup
}

// NewDispatcher sends to every channel given.
func NewDispatcher(channels ...Channel) *Dispatcher {
	return &Dispatcher{channels: channels, timeout: 10 * time.Second, nextID: 1}
}

// Channels names the configured channels.
func (d *Dispatcher) Channels() []string {
	names := make([]string, len(d.channels))
	for i, c := range d.channels {
		names[i] = c.Name()
	}
	return names
}

// Notify implements monitor.Notifier.
func (d *Dispatcher) Notify(tr monitor.Transition) {
	m := Compose(tr)
	d.mu.Lock()
	epoch := d.epoch
	d.mu.Unlock()
	for _, c := range d.channels {
		d.wg.Add(1)
		go func(c Channel) {
			defer d.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
			defer cancel()
			err := c.Send(ctx, m)
			d.record(epoch, c.Name(), m, err)
		}(c)
	}
}

func (d *Dispatcher) record(epoch int, channel string, m Message, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if epoch != d.epoch {
		return
	}
	del := Delivery{
		ID: d.nextID, At: time.Now().UTC(), Channel: channel, Kind: m.Kind,
		TargetID: m.Transition.Target.ID, TargetName: m.Transition.Target.Name,
		Subject: m.Subject, Delivered: err == nil,
	}
	if err != nil {
		del.Error = err.Error()
	}
	d.nextID++
	d.deliveries = append(d.deliveries, del)
	if len(d.deliveries) > maxDeliveries {
		d.deliveries = d.deliveries[len(d.deliveries)-maxDeliveries:]
	}
}

// Deliveries returns the delivery record, newest first.
func (d *Dispatcher) Deliveries() []Delivery {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Delivery, len(d.deliveries))
	for i, del := range d.deliveries {
		out[len(out)-1-i] = del
	}
	return out
}

// Reset forgets the delivery record. Sends already in flight still complete
// but are not recorded.
func (d *Dispatcher) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deliveries = nil
	d.nextID = 1
	d.epoch++
}

// Wait blocks until every send in flight has finished.
func (d *Dispatcher) Wait() { d.wg.Wait() }

// Compose writes the message for a transition, in words a person woken by it
// can act on.
func Compose(tr monitor.Transition) Message {
	inc := tr.Incident
	name := tr.Target.Name
	const layout = "2006-01-02 15:04:05 MST"
	if tr.To == monitor.Down {
		return Message{
			Kind:    "down",
			Subject: fmt.Sprintf("[DOWN] %s is down", name),
			Text: fmt.Sprintf("%s (%s %s) has been failing since %s. Confirmed after %d consecutive failed checks at %s. %s",
				name, tr.Target.Type, tr.Target.Address,
				inc.StartedAt.Format(layout), inc.FailedChecks, inc.DetectedAt.Format(layout), lastDetail(inc)),
			Transition: tr,
		}
	}
	text := fmt.Sprintf("%s (%s %s) is back up, confirmed at %s.", name, tr.Target.Type, tr.Target.Address, tr.At.Format(layout))
	if inc.EndedAt != nil {
		text = fmt.Sprintf("%s The outage lasted %s, from %s to %s.", text,
			inc.EndedAt.Sub(inc.StartedAt).Round(time.Second), inc.StartedAt.Format(layout), inc.EndedAt.Format(layout))
	}
	return Message{Kind: "recovered", Subject: fmt.Sprintf("[RECOVERED] %s is back up", name), Text: text, Transition: tr}
}

func lastDetail(inc monitor.Incident) string {
	if n := len(inc.Observations); n > 0 {
		return "Last check: " + inc.Observations[n-1].Detail + "."
	}
	return ""
}
