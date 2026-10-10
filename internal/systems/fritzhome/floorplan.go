package fritzhome

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"armdash/internal/fritzbox"
	"armdash/internal/sh3d"
	"armdash/internal/system"
)

const examplePlan = `{
  "width": 800, "height": 500,
  "rooms": [
    {"name": "Living room", "points": "20,20 400,20 400,300 20,300"},
    {"name": "Kitchen",     "points": "400,20 780,20 780,180 400,180"}
  ],
  "devices": [
    {"ain": "116300343807", "x": 120, "y": 160},
    {"ain": "139790212573", "x": 300, "y": 240}
  ]
}`

// jsonPlan is the hand-written floor plan. Deliberately plain geometry so it
// can be traced from a sketch by hand: SVG polygon/polyline point strings, and
// devices placed by AIN so the drawing never repeats a device name.
type jsonPlan struct {
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Outline string `json:"outline"` // the flat's outer wall
	Rooms   []struct {
		Name   string  `json:"name"`
		Points string  `json:"points"`
		LabelX float64 `json:"labelX"`
		LabelY float64 `json:"labelY"`
	} `json:"rooms"`
	Walls []string `json:"walls"` // interior walls, as polyline point strings
	Doors []struct {
		X1 float64 `json:"x1"`
		Y1 float64 `json:"y1"`
		X2 float64 `json:"x2"`
		Y2 float64 `json:"y2"`
	} `json:"doors"`
	Devices []struct {
		AIN   string  `json:"ain"`
		Label string  `json:"label"`
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
	} `json:"devices"`
}

// view is a floor plan ready to draw, whichever format it came from.
type view struct {
	MinX, MinY, W, H float64
	// Traced walls are thin strokes inside a separate outline; SweetHome3D
	// walls are the real thing at their real thickness, so they draw darker
	// and with square ends that close the corners.
	WallClass, WallCap string
	Outline            string
	Rooms              []viewRoom
	Walls              []viewWall
	Doors, Windows     []segment
	Furniture          []viewPiece
	Devices            []placement
	// Image is an uploaded picture, drawn as the whole plan.
	Image string
}

type viewRoom struct {
	Name, Points, Anchor string
	LabelX, LabelY       float64
}

type viewWall struct {
	Points string
	Width  float64
}

type segment struct{ X1, Y1, X2, Y2, Width float64 }

// viewPiece is a furniture footprint: X and Y its unrotated top-left corner,
// turned by Angle degrees about its centre CX, CY.
type viewPiece struct {
	Name                      string
	X, Y, W, H, Angle, CX, CY float64
}

type placement struct {
	AIN, Label string
	X, Y       float64
}

// loadPlan returns the plan in use and where it came from. An upload wins over
// floorplan_file, which wins over the inline value. No plan at all is a nil
// view and no error.
func (f *FritzHome) loadPlan() (*view, string, error) {
	if f.dataDir != "" {
		path := filepath.Join(f.dataDir, uploadName)
		if st, err := os.Stat(path); err == nil {
			v, err := f.planFromUpload(path, st)
			return v, "uploaded " + st.ModTime().Format("2 Jan 15:04"), err
		}
	}
	if path := f.deps.Config.Get("floorplan_file"); path != "" {
		v, err := planFromFile(path)
		return v, "from floorplan_file", err
	}
	if raw := f.deps.Config.Get("floorplan"); strings.TrimSpace(raw) != "" {
		v, err := planFromJSON([]byte(raw))
		return v, "from the inline setting", err
	}
	return nil, "", nil
}

// planFromFile picks the format by content, not by extension: a ZIP is
// SweetHome3D, and JSON cannot start with "PK". Only Home.xml is read out of an
// archive, so a drawing full of textures costs no more than a bare one.
func planFromFile(path string) (*view, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading floor plan: %w", err)
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	magic := make([]byte, 2)
	if _, err := fh.ReadAt(magic, 0); err == nil && string(magic) == "PK" {
		h, err := sh3d.Read(fh, st.Size())
		if err != nil {
			return nil, fmt.Errorf("floor plan %s: %w", filepath.Base(path), err)
		}
		return planFromSH3D(h.FirstLevel()), nil
	}
	b, err := io.ReadAll(fh)
	if err != nil {
		return nil, fmt.Errorf("reading floor plan: %w", err)
	}
	return planFromJSON(b)
}

func planFromJSON(b []byte) (*view, error) {
	var p jsonPlan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("floor plan JSON is invalid: %w", err)
	}
	v := &view{W: 800, H: 500, WallClass: "fp-wall", WallCap: "round", Outline: p.Outline}
	if p.Width > 0 {
		v.W = float64(p.Width)
	}
	if p.Height > 0 {
		v.H = float64(p.Height)
	}
	for _, rm := range p.Rooms {
		lx, ly := rm.LabelX, rm.LabelY
		if lx == 0 && ly == 0 {
			x, y := firstPoint(rm.Points)
			lx, ly = x+12, y+26
		}
		v.Rooms = append(v.Rooms, viewRoom{Name: rm.Name, Points: rm.Points, Anchor: "start", LabelX: lx, LabelY: ly})
	}
	for _, w := range p.Walls {
		v.Walls = append(v.Walls, viewWall{Points: w, Width: 3})
	}
	for _, d := range p.Doors {
		v.Doors = append(v.Doors, segment{d.X1, d.Y1, d.X2, d.Y2, 5})
	}
	for _, d := range p.Devices {
		v.Devices = append(v.Devices, placement{AIN: d.AIN, Label: d.Label, X: d.X, Y: d.Y})
	}
	return v, nil
}

func planFromSH3D(h *sh3d.Home) *view {
	v := &view{WallClass: "fp-outline", WallCap: "square"}
	var b bounds
	for _, w := range h.Walls {
		v.Walls = append(v.Walls, viewWall{
			Points: points(sh3d.Point{X: w.XStart, Y: w.YStart}, sh3d.Point{X: w.XEnd, Y: w.YEnd}),
			Width:  r2(w.Thickness),
		})
		b.add(w.XStart, w.YStart, w.Thickness/2)
		b.add(w.XEnd, w.YEnd, w.Thickness/2)
	}
	for _, r := range h.Rooms {
		if len(r.Points) < 3 {
			continue
		}
		var rb bounds
		for _, p := range r.Points {
			rb.add(p.X, p.Y, 0)
			b.add(p.X, p.Y, 0)
		}
		// SweetHome3D centres the name on the room's bounding box, then applies
		// the offset the user dragged it by.
		v.Rooms = append(v.Rooms, viewRoom{
			Name: r.Name, Points: points(r.Points...), Anchor: "middle",
			LabelX: r2((rb.minX+rb.maxX)/2 + r.NameXOffset),
			LabelY: r2((rb.minY+rb.maxY)/2 + r.NameYOffset),
		})
	}
	for _, p := range h.Furniture {
		if p.DoorOrWindow {
			s := opening(p)
			b.add(s.X1, s.Y1, 0)
			b.add(s.X2, s.Y2, 0)
			// SweetHome3D does not record which is which, and the catalogue
			// names are localised. A door is an opening that starts at the floor.
			if p.Elevation < 1 {
				v.Doors = append(v.Doors, s)
			} else {
				v.Windows = append(v.Windows, s)
			}
			continue
		}
		sin, cos := math.Sincos(p.Angle)
		for _, c := range [4][2]float64{{-1, -1}, {1, -1}, {1, 1}, {-1, 1}} {
			lx, ly := c[0]*p.Width/2, c[1]*p.Depth/2
			b.add(p.X+lx*cos-ly*sin, p.Y+lx*sin+ly*cos, 0)
		}
		v.Furniture = append(v.Furniture, viewPiece{
			Name: p.Name,
			X:    r2(p.X - p.Width/2), Y: r2(p.Y - p.Depth/2), W: r2(p.Width), H: r2(p.Depth),
			Angle: r2(p.Angle * 180 / math.Pi), CX: r2(p.X), CY: r2(p.Y),
		})
	}
	if !b.ok {
		v.W, v.H = 800, 500
		return v
	}
	const margin = 30
	v.MinX, v.MinY = r2(b.minX-margin), r2(b.minY-margin)
	v.W, v.H = r2(b.maxX-b.minX+2*margin), r2(b.maxY-b.minY+2*margin)
	return v
}

// opening is the slice of a door or window that sits inside its wall, as a
// segment along the wall's centre line. SweetHome3D stores the whole
// footprint, which for a door includes the leaf standing proud of the wall;
// WallDistance and WallThickness say which part of the depth is wall.
func opening(p sh3d.Piece) segment {
	off := p.Depth * (p.WallDistance + p.WallThickness/2 - 0.5)
	sin, cos := math.Sincos(p.Angle)
	cx, cy := p.X-off*sin, p.Y+off*cos
	dx, dy := cos*p.Width/2, sin*p.Width/2
	// Two units wider than the wall so no sliver of wall shows along its edges.
	return segment{r2(cx - dx), r2(cy - dy), r2(cx + dx), r2(cy + dy), r2(p.Depth*p.WallThickness + 2)}
}

type bounds struct {
	minX, minY, maxX, maxY float64
	ok                     bool
}

func (b *bounds) add(x, y, pad float64) {
	if !b.ok {
		*b = bounds{x - pad, y - pad, x + pad, y + pad, true}
		return
	}
	b.minX, b.minY = min(b.minX, x-pad), min(b.minY, y-pad)
	b.maxX, b.maxY = max(b.maxX, x+pad), max(b.maxY, y+pad)
}

// r2 rounds to hundredths of a unit, a tenth of a millimetre in a drawing,
// which keeps the served SVG readable. Adding zero turns a -0 into 0.
func r2(v float64) float64 { return math.Round(v*100)/100 + 0 }

func num(v float64) string { return strconv.FormatFloat(r2(v), 'f', -1, 64) }

func points(ps ...sh3d.Point) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = num(p.X) + "," + num(p.Y)
	}
	return strings.Join(s, " ")
}

func viewBox(x, y, w, h float64) string {
	return num(x) + " " + num(y) + " " + num(w) + " " + num(h)
}

func firstPoint(points string) (float64, float64) {
	fields := strings.Fields(points)
	if len(fields) == 0 {
		return 0, 0
	}
	var x, y float64
	_, _ = fmt.Sscanf(fields[0], "%f,%f", &x, &y)
	return x, y
}

type marker struct {
	AIN, Name, Value, Unit, Colour string
	Temp                           string // a plug's temperature, under its power
	X, Y                           float64
	Placed                         bool
}

type floorplanPage struct {
	Top                system.PageTop
	Err                string
	HasPlan, Editable  bool
	Example, API       string
	View               *view
	ViewBox, EditBox   string
	PlanBottom, StripH float64
	Markers            []marker
}

func (f *FritzHome) renderFloorplan(r *http.Request) (template.HTML, error) {
	d := floorplanPage{Example: examplePlan, Editable: f.dataDir != "" && system.CanEdit(r), API: f.prefix}
	d.Top = system.PageTop{Title: "Floor plan"}
	d.Top.Infof(`<span class="tag is-primary is-light">power</span>`)
	d.Top.Infof(`<span class="tag is-link is-light">temperature</span>`)
	d.Top.Infof(`<span class="tag is-danger is-light">doors</span>`)
	if d.Editable {
		if err := f.action(&d.Top, "fp-upload", d); err != nil {
			return "", err
		}
	}

	// A plan that fails to load is shown as an error on the page rather than
	// failing it, so the upload button stays there to replace it.
	v, source, err := f.loadPlan()
	if err != nil {
		d.Err = err.Error()
	}
	if v == nil {
		return f.exec("floorplan", d)
	}
	d.HasPlan, d.View = true, v
	d.Top.Infof(`<span class="has-text-grey is-size-7">%s</span>`, template.HTMLEscapeString(source))
	if d.Editable {
		if err := f.action(&d.Top, "fp-edit", d); err != nil {
			return "", err
		}
	}

	// Saved positions replace the plan's own entirely, so a device taken off
	// the plan in edit mode stays off. Labels from a JSON plan still apply.
	placements := v.Devices
	saved, err := f.savedPositions()
	if err != nil {
		d.Err = err.Error()
	}
	if saved != nil {
		labels := map[string]string{}
		for _, p := range v.Devices {
			labels[p.AIN] = p.Label
		}
		placements = nil
		for ain, s := range saved {
			placements = append(placements, placement{AIN: ain, Label: labels[ain], X: s.X, Y: s.Y})
		}
		sort.Slice(placements, func(i, j int) bool { return placements[i].AIN < placements[j].AIN })
	}

	var devices []fritzbox.Device
	if f.configured() {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		devices, _ = f.devices(ctx)
	}
	byAIN := map[string]fritzbox.Device{}
	for _, dv := range devices {
		byAIN[dv.AIN] = dv
	}

	// A device the box reports but the plan has not placed waits in a strip
	// below the plan, which only edit mode shows. So does one saved outside
	// the current plan, typically after a drawing was replaced by a picture of
	// another size; off-screen it could never be dragged back.
	var strip []marker
	placed := map[string]bool{}
	for _, p := range placements {
		m := newMarker(p.AIN, p.Label, byAIN)
		placed[p.AIN] = true
		if p.X < v.MinX || p.X > v.MinX+v.W || p.Y < v.MinY || p.Y > v.MinY+v.H {
			strip = append(strip, m)
			continue
		}
		m.X, m.Y, m.Placed = p.X, p.Y, true
		d.Markers = append(d.Markers, m)
	}
	for _, dv := range devices {
		if !placed[dv.AIN] {
			strip = append(strip, newMarker(dv.AIN, "", byAIN))
		}
	}
	d.PlanBottom = r2(v.MinY + v.H)
	const slot = 90
	perRow := max(1, int(v.W/slot))
	for i, m := range strip {
		m.X = r2(v.MinX + slot/2 + float64(i%perRow)*slot)
		m.Y = r2(d.PlanBottom + 85 + float64(i/perRow)*slot)
		d.Markers = append(d.Markers, m)
	}
	d.StripH = 40 + float64(max(1, (len(strip)+perRow-1)/perRow))*slot
	d.ViewBox = viewBox(v.MinX, v.MinY, v.W, v.H)
	d.EditBox = viewBox(v.MinX, v.MinY, v.W, v.H+d.StripH)
	return f.exec("floorplan", d)
}

func newMarker(ain, label string, byAIN map[string]fritzbox.Device) marker {
	m := marker{AIN: ain, Name: label, Value: "n/a", Colour: "hsl(0, 0%, 71%)"}
	d, ok := byAIN[ain]
	if !ok {
		if m.Name == "" {
			m.Name = ain
		}
		return m
	}
	if m.Name == "" {
		m.Name = d.Name
	}
	// Power is the more interesting number when a device reports both.
	switch {
	case d.PowerW != nil:
		m.Value = fmt.Sprintf("%.0f", *d.PowerW)
		m.Unit = "W"
		m.Colour = "hsl(171, 100%, 41%)"
		if d.TempC != nil {
			m.Temp = fmt.Sprintf("%.1f °C", *d.TempC)
		}
	case d.TempC != nil:
		m.Value = fmt.Sprintf("%.1f", *d.TempC)
		m.Unit = "°C"
		m.Colour = "hsl(229, 53%, 53%)"
	}
	if !d.Present {
		m.Colour = "hsl(348, 86%, 61%)"
	}
	return m
}

// action renders one of the page's own templates into the infobar, so a form's
// markup stays in the template file rather than in a Go string.
func (f *FritzHome) action(top *system.PageTop, name string, data any) error {
	h, err := f.exec(name, data)
	if err != nil {
		return err
	}
	top.Actions = append(top.Actions, h)
	return nil
}
