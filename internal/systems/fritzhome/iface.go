package fritzhome

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"armdash/internal/fritzbox"
	"armdash/internal/system"
)

// Smart home devices can be read over AHA, which every box speaks, or over the
// REST API of FRITZ!OS 8.20 and later. AHA is the default: the REST API is a
// 0.9 specification, and on the box it was checked against it leaves out a
// meter reader's energy total.
const (
	restKey     = "rest_api"
	fallbackKey = "rest_fallback" // "<RFC 3339 time> <reason>", not a form field
	gapEvery    = time.Hour
)

// reader is the part of the client devices() needs, so the choice between
// the two interfaces is testable without a box.
type reader interface {
	Devices(ctx context.Context) ([]fritzbox.Device, error)
	DevicesREST(ctx context.Context) ([]fritzbox.Device, error)
}

type ifaceState struct {
	mu sync.Mutex
	// restOff is set once REST has failed while AHA worked. Normally the
	// saved setting makes this instance obsolete at once; when an env file
	// fixes the setting, this keeps the instance on AHA until a restart.
	restOff bool
	cleared bool
	gaps    []string
	gapAt   time.Time
}

func (f *FritzHome) wantREST() bool { return f.deps.Config.Bool(restKey) }

// read fetches devices over the chosen interface. A REST failure is retried
// over AHA at once: when AHA answers, the box is fine and the REST API is the
// problem, so the setting goes back to AHA, with the reason kept for the
// status box. When AHA fails too, the box itself is down or the login is
// wrong, and switching would be wrong; the REST error is reported.
func (f *FritzHome) read(ctx context.Context, cli reader) ([]fritzbox.Device, error) {
	f.iface.mu.Lock()
	off := f.iface.restOff
	f.iface.mu.Unlock()
	if !f.wantREST() || off {
		return cli.Devices(ctx)
	}

	devices, restErr := cli.DevicesREST(ctx)
	if restErr == nil {
		f.restWorked(ctx, cli, devices)
		return devices, nil
	}
	aha, err := cli.Devices(ctx)
	if err != nil {
		return nil, restErr
	}
	f.fallBack(restErr)
	return aha, nil
}

func (f *FritzHome) fallBack(reason error) {
	f.iface.mu.Lock()
	f.iface.restOff = true
	f.iface.mu.Unlock()
	f.deps.Log.Warn("REST API failed, back on AHA", "err", reason)
	if f.deps.Save == nil {
		return
	}
	note := time.Now().Format(time.RFC3339) + " " + reason.Error()
	if err := f.deps.Save(map[string]string{restKey: "", fallbackKey: note}); err != nil {
		f.deps.Log.Warn("could not switch the setting back to AHA, staying on AHA until restart", "err", err)
	}
}

// restWorked drops a reason from an earlier fall back, once the owner has
// turned the REST API on again and it answers, and compares it with AHA once
// an hour.
func (f *FritzHome) restWorked(ctx context.Context, cli reader, rest []fritzbox.Device) {
	f.iface.mu.Lock()
	clear := !f.iface.cleared && f.deps.Config.Get(fallbackKey) != ""
	f.iface.cleared = true
	compare := time.Since(f.iface.gapAt) >= gapEvery
	f.iface.mu.Unlock()

	if clear && f.deps.Save != nil {
		if err := f.deps.Save(map[string]string{fallbackKey: ""}); err != nil {
			f.deps.Log.Warn("clearing the REST fall back note", "err", err)
		}
	}
	if !compare {
		return
	}
	aha, err := cli.Devices(ctx)
	f.iface.mu.Lock()
	defer f.iface.mu.Unlock()
	f.iface.gapAt = time.Now()
	if err == nil {
		f.iface.gaps = fritzbox.Missing(aha, rest)
	}
}

// gaps is what AHA reports and the REST API does not, while REST is in use.
func (f *FritzHome) gaps() []string {
	if !f.wantREST() {
		return nil
	}
	f.iface.mu.Lock()
	defer f.iface.mu.Unlock()
	if f.iface.restOff {
		return nil
	}
	return f.iface.gaps
}

// ifaceCheck is the status box line on which interface is in use.
func (f *FritzHome) ifaceCheck() []system.Check {
	f.iface.mu.Lock()
	off := f.iface.restOff
	f.iface.mu.Unlock()
	if f.wantREST() && !off {
		out := []system.Check{{Title: "Smart home interface", Level: "ok", Detail: "REST API"}}
		if g := f.gaps(); len(g) > 0 {
			out = append(out, system.Check{Title: "REST API gaps", Level: "warning",
				Detail: "AHA reports these and the REST API does not: " + strings.Join(g, "; ") +
					". Turn the REST API off to get them back."})
		}
		return out
	}
	// A fall back stays in view for a week, then it is history.
	if at, reason, ok := fallbackNote(f.deps.Config.Get(fallbackKey)); ok {
		level := "ok"
		if time.Since(at) < 7*24*time.Hour {
			level = "warning"
		}
		return []system.Check{{Title: "Smart home interface", Level: level,
			Detail: fmt.Sprintf("AHA. Switched back from the REST API on %s: %s", at.Local().Format("2 Jan, 15:04"), reason)}}
	}
	return []system.Check{{Title: "Smart home interface", Level: "ok", Detail: "AHA"}}
}

func fallbackNote(v string) (time.Time, string, bool) {
	at, reason, ok := strings.Cut(v, " ")
	if !ok {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return time.Time{}, "", false
	}
	return t, reason, true
}
