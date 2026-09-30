package fritzhome

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"armdash/internal/fritzbox"
	"armdash/internal/system"
)

type memConfig map[string]string

func (m memConfig) Get(k string) string { return m[k] }
func (m memConfig) GetOr(k, def string) string {
	if v := m[k]; v != "" {
		return v
	}
	return def
}
func (m memConfig) Bool(k string) bool { return m[k] == "1" }

type fakeBox struct {
	aha, rest   []fritzbox.Device
	ahaErr      error
	restErr     error
	ahaN, restN int
}

func (b *fakeBox) Devices(context.Context) ([]fritzbox.Device, error) {
	b.ahaN++
	return b.aha, b.ahaErr
}

func (b *fakeBox) DevicesREST(context.Context) ([]fritzbox.Device, error) {
	b.restN++
	return b.rest, b.restErr
}

// newIfaceFritz saves into the same map it reads, as the shell does.
func newIfaceFritz(cfg memConfig) (*FritzHome, *[]map[string]string) {
	var saves []map[string]string
	f := &FritzHome{deps: system.Deps{
		Config: cfg,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Save: func(c map[string]string) error {
			saves = append(saves, c)
			for k, v := range c {
				cfg[k] = v
			}
			return nil
		},
	}}
	return f, &saves
}

var (
	plug  = fritzbox.Device{AIN: "1", Name: "Plug", PowerW: new(float64)}
	meter = fritzbox.Device{AIN: "2", Name: "Meter", EnergyKWh: new(float64)}
)

func TestAHAIsTheDefault(t *testing.T) {
	f, _ := newIfaceFritz(memConfig{})
	box := &fakeBox{aha: []fritzbox.Device{plug}}
	if _, err := f.read(context.Background(), box); err != nil || box.restN != 0 {
		t.Errorf("read REST %d times without the setting (err %v)", box.restN, err)
	}
}

func TestRESTFailingWithAHAWorkingSwitchesBack(t *testing.T) {
	cfg := memConfig{restKey: "1"}
	f, saves := newIfaceFritz(cfg)
	box := &fakeBox{aha: []fritzbox.Device{plug}, restErr: errors.New("HTTP 500 from /api/v0")}

	got, err := f.read(context.Background(), box)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v; want the AHA reading", got, err)
	}
	if len(*saves) != 1 || cfg[restKey] != "" || !strings.Contains(cfg[fallbackKey], "HTTP 500") {
		t.Fatalf("saves %v, want the setting off and the reason kept", *saves)
	}
	if _, err := f.read(context.Background(), box); err != nil || box.restN != 1 {
		t.Errorf("REST was tried again after falling back (%d)", box.restN)
	}
	if c := f.ifaceCheck(); c[0].Level != "warning" || !strings.Contains(c[0].Detail, "HTTP 500") {
		t.Errorf("status says %+v", c)
	}
}

// Both failing is the box or the login, not the REST API.
func TestBothFailingKeepsTheSetting(t *testing.T) {
	cfg := memConfig{restKey: "1"}
	f, saves := newIfaceFritz(cfg)
	box := &fakeBox{ahaErr: errors.New("no route"), restErr: errors.New("no route to REST")}
	if _, err := f.read(context.Background(), box); err == nil || !strings.Contains(err.Error(), "REST") {
		t.Errorf("err = %v, want the REST error", err)
	}
	if len(*saves) != 0 || cfg[restKey] != "1" {
		t.Errorf("a dead box switched the setting: %v", *saves)
	}
}

func TestWorkingRESTClearsTheOldReasonAndReportsGaps(t *testing.T) {
	cfg := memConfig{restKey: "1", fallbackKey: "2026-10-02T14:03:00Z HTTP 500"}
	f, saves := newIfaceFritz(cfg)
	withTotal := meter
	withTotal.EnergyKWh = new(float64)
	box := &fakeBox{aha: []fritzbox.Device{plug, withTotal}, rest: []fritzbox.Device{plug, {AIN: "2", Name: "Meter"}}}

	for range 3 {
		if _, err := f.read(context.Background(), box); err != nil {
			t.Fatal(err)
		}
	}
	if cfg[fallbackKey] != "" || len(*saves) != 1 {
		t.Errorf("saves %v, want the old reason cleared once", *saves)
	}
	if box.ahaN != 1 {
		t.Errorf("AHA read %d times for the comparison, want once an hour", box.ahaN)
	}
	if g := f.gaps(); strings.Join(g, "; ") != "Meter: energy" {
		t.Errorf("gaps %q", g)
	}
	if cfg[restKey] != "1" {
		t.Error("a working REST API was switched off")
	}
}
