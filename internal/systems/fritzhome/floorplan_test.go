package fritzhome

import (
	"archive/zip"
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"armdash/internal/config"
	"armdash/internal/fritzbox"
	"armdash/internal/sh3d"
	"armdash/internal/system"
)

func sh3dFile(t *testing.T, homeXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("Home.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(homeXML)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// One square room. The door's footprint stands 4 units proud of the wall on
// y=400, the window is turned to sit in the wall on x=0, the table is turned
// on its side.
const testHome = `<home>
  <wall xStart='0' yStart='0' xEnd='400' yEnd='0' thickness='10'/>
  <wall xStart='0' yStart='400' xEnd='400' yEnd='400' thickness='8'/>
  <room name='Hall'><point x='0' y='0'/><point x='400' y='0'/><point x='400' y='400'/><point x='0' y='400'/></room>
  <doorOrWindow name='Door' x='200' y='404' width='80' depth='20' wallThickness='0.4' wallDistance='0.1'/>
  <doorOrWindow name='Window' x='0' y='200' elevation='90' angle='1.5707964' width='100' depth='10'/>
  <pieceOfFurniture name='Table' x='100' y='100' width='60' depth='40' angle='1.5707964'/>
</home>`

func testView(t *testing.T) *view {
	t.Helper()
	b := sh3dFile(t, testHome)
	h, err := sh3d.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return planFromSH3D(h.FirstLevel())
}

func TestSH3DOpeningsSitOnTheWall(t *testing.T) {
	v := testView(t)
	if len(v.Doors) != 1 || len(v.Windows) != 1 {
		t.Fatalf("doors %v, windows %v: want one of each, told apart by elevation", v.Doors, v.Windows)
	}
	if got, want := v.Doors[0], (segment{160, 400, 240, 400, 10}); got != want {
		t.Errorf("door = %+v, want %+v: the slice inside the wall, on its centre line", got, want)
	}
	if got, want := v.Windows[0], (segment{0, 150, 0, 250, 12}); got != want {
		t.Errorf("window = %+v, want %+v", got, want)
	}
}

func TestSH3DView(t *testing.T) {
	v := testView(t)
	if v.MinX != -35 || v.MinY != -35 || v.W != 470 || v.H != 469 {
		t.Errorf("view box = %v %v %v %v, want the walls' outer edges plus the margin", v.MinX, v.MinY, v.W, v.H)
	}
	if len(v.Walls) != 2 || v.Walls[0].Points != "0,0 400,0" || v.Walls[0].Width != 10 || v.WallCap != "square" {
		t.Errorf("walls = %+v, cap %q", v.Walls, v.WallCap)
	}
	if r := v.Rooms[0]; r.Name != "Hall" || r.LabelX != 200 || r.LabelY != 200 || r.Anchor != "middle" {
		t.Errorf("room = %+v, want the label centred", r)
	}
	if got, want := v.Furniture[0], (viewPiece{"Table", 70, 80, 60, 40, 90, 100, 100}); got != want {
		t.Errorf("table = %+v, want %+v", got, want)
	}
}

func TestJSONPlanKeepsItsShape(t *testing.T) {
	v, err := planFromJSON([]byte(`{
  "outline": "0,0 10,0 10,10",
  "rooms": [{"name": "Desk", "points": "20,30 40,30 40,60"}, {"name": "Bed", "points": "0,0", "labelX": 5, "labelY": 6}],
  "walls": ["1,1 2,2"],
  "doors": [{"x1": 1, "y1": 2, "x2": 3, "y2": 4}],
  "devices": [{"ain": "116570593149", "label": "Desk", "x": 150, "y": 130}]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.MinX != 0 || v.MinY != 0 || v.W != 800 || v.H != 500 {
		t.Errorf("view box = %v %v %v %v, want the 800x500 default", v.MinX, v.MinY, v.W, v.H)
	}
	if r := v.Rooms[0]; r.LabelX != 32 || r.LabelY != 56 || r.Anchor != "start" {
		t.Errorf("room label = %+v, want it tucked into the first corner", r)
	}
	if r := v.Rooms[1]; r.LabelX != 5 || r.LabelY != 6 {
		t.Errorf("room label = %+v, want the explicit position", r)
	}
	if v.Walls[0].Width != 3 || v.WallCap != "round" || v.WallClass != "fp-wall" || v.Doors[0].Width != 5 {
		t.Errorf("walls %+v doors %+v: traced plans keep their thin strokes", v.Walls, v.Doors)
	}
	if v.Devices[0] != (placement{"116570593149", "Desk", 150, 130}) {
		t.Errorf("device = %+v", v.Devices[0])
	}
}

func newTestFritz(t *testing.T, dataDir, planFile string) (*FritzHome, *http.ServeMux) {
	t.Helper()
	// No password keeps the tests away from any real box.
	t.Setenv("AD_SYSTEM_FRITZHOME_PASSWORD", "")
	t.Setenv("AD_SYSTEM_FRITZHOME_FLOORPLAN", "")
	t.Setenv("AD_SYSTEM_FRITZHOME_FLOORPLAN_FILE", planFile)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	f := &FritzHome{}
	mux := http.NewServeMux()
	f.Register(mux, "/s/fritzhome/api/", system.Deps{
		Config:  cfg.Scoped("fritzhome"),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir: dataDir,
	})
	return f, mux
}

func upload(mux *http.ServeMux, body []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("plan", "flat.sh3d")
	_, _ = fw.Write(body)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/s/fritzhome/api/floorplan", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func post(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

// page renders the floor plan as the logged-in owner sees it.
func page(t *testing.T, f *FritzHome) string {
	t.Helper()
	return render(t, f, system.AllowEdit(httptest.NewRequest(http.MethodGet, "/s/fritzhome/floorplan", nil)))
}

func render(t *testing.T, f *FritzHome, r *http.Request) string {
	t.Helper()
	h, err := f.Render("floorplan", r)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func TestUploadReplacesThePlan(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fritzhome")
	f, mux := newTestFritz(t, dir, "")
	first := sh3dFile(t, testHome)
	plan := filepath.Join(dir, uploadName)

	rec := upload(mux, first)
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("upload = %d %q, want 200 and a page refresh", rec.Code, rec.Body.String())
	}
	if b, _ := os.ReadFile(plan); !bytes.Equal(b, first) {
		t.Fatal("the uploaded file was not stored")
	}

	for body, want := range map[string]string{
		`{"width": 800}`:                 "not a SweetHome3D file",
		string(sh3dFile(t, "<home/>")):   "no walls or rooms",
		string(sh3dFile(t, "<plan/>")):   "not &lt;home&gt;",
		string(sh3dFile(t, "<home><wa")): "parsing",
	} {
		rec := upload(mux, []byte(body))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("bad upload = %d %q, want 422 mentioning %q", rec.Code, rec.Body.String(), want)
		}
		if b, _ := os.ReadFile(plan); !bytes.Equal(b, first) {
			t.Fatal("a rejected upload touched the stored plan")
		}
	}

	second := sh3dFile(t, strings.Replace(testHome, "Hall", "Lobby", 1))
	if rec := upload(mux, second); rec.Code != http.StatusOK {
		t.Fatalf("second upload = %d", rec.Code)
	}
	if b, _ := os.ReadFile(plan + ".prev"); !bytes.Equal(b, first) {
		t.Error("the previous plan was not kept")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("data directory holds %d entries, want the plan and its backup, no temp files", len(entries))
	}

	html := page(t, f)
	for _, want := range []string{"Lobby", "uploaded ", `id="fp-edit"`, `hx-post="/s/fritzhome/api/floorplan"`} {
		if !strings.Contains(html, want) {
			t.Errorf("page is missing %s", want)
		}
	}
}

func TestUploadWinsOverTheConfiguredFile(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(planFile, []byte(examplePlan), 0o600); err != nil {
		t.Fatal(err)
	}
	f, mux := newTestFritz(t, filepath.Join(dir, "data"), planFile)
	if v, src, err := f.loadPlan(); err != nil || src != "from floorplan_file" || v.WallCap != "round" {
		t.Fatalf("before upload: %q %v, want the JSON file", src, err)
	}
	upload(mux, sh3dFile(t, testHome))
	if v, src, err := f.loadPlan(); err != nil || !strings.HasPrefix(src, "uploaded") || v.WallCap != "square" {
		t.Fatalf("after upload: %q %v, want the uploaded drawing", src, err)
	}
}

func TestSavedPositionsPlaceDevices(t *testing.T) {
	dir := t.TempDir()
	f, mux := newTestFritz(t, dir, "")
	upload(mux, sh3dFile(t, testHome))

	if rec := post(mux, "/s/fritzhome/api/positions", `{"116570593149": {"x": 10, "y": 20}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("save = %d %q", rec.Code, rec.Body.String())
	}
	html := page(t, f)
	if !strings.Contains(html, `data-ain="116570593149"`) || !strings.Contains(html, `translate(10 20)`) {
		t.Error("a saved position is not on the page")
	}

	// Taking every device off is a saved state too, not a return to the plan's own.
	if rec := post(mux, "/s/fritzhome/api/positions", `{}`); rec.Code != http.StatusNoContent {
		t.Fatalf("save = %d", rec.Code)
	}
	if saved, err := f.savedPositions(); err != nil || saved == nil || len(saved) != 0 {
		t.Errorf("saved = %v, %v, want an empty, non-nil map", saved, err)
	}

	for _, body := range []string{`null`, `[]`, `{"<b>": {"x": 1, "y": 1}}`, `{"1": {"x": 1e9, "y": 1}}`, `{"1": {"x": "a"}}`} {
		if rec := post(mux, "/s/fritzhome/api/positions", body); rec.Code != http.StatusBadRequest {
			t.Errorf("save %s = %d, want 400", body, rec.Code)
		}
	}
}

func TestWithoutDataDirNothingIsWritable(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(planFile, []byte(examplePlan), 0o600); err != nil {
		t.Fatal(err)
	}
	f, mux := newTestFritz(t, "", planFile)
	if rec := upload(mux, sh3dFile(t, testHome)); rec.Code != http.StatusNotFound {
		t.Errorf("upload = %d, want 404", rec.Code)
	}
	if rec := post(mux, "/s/fritzhome/api/positions", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("save = %d, want 404", rec.Code)
	}
	html := page(t, f)
	if strings.Contains(html, "hx-post") || strings.Contains(html, "fp-edit\"") {
		t.Error("upload or edit offered without a data directory")
	}
	if !strings.Contains(html, `data-ain="116300343807"`) {
		t.Error("the JSON plan's own devices are missing")
	}
}

func TestEditControlsNeedLogin(t *testing.T) {
	f, mux := newTestFritz(t, t.TempDir(), "")
	upload(mux, sh3dFile(t, testHome))
	html := render(t, f, httptest.NewRequest(http.MethodGet, "/s/fritzhome/floorplan", nil))
	if strings.Contains(html, "hx-post") || strings.Contains(html, `id="fp-edit"`) {
		t.Error("upload or edit offered to a visitor who is not logged in")
	}
	if !strings.Contains(html, "Hall") {
		t.Error("the plan itself should still be shown")
	}
}

func TestBrokenPlanKeepsTheUploadButton(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(planFile, []byte(`{"width": `), 0o600); err != nil {
		t.Fatal(err)
	}
	f, _ := newTestFritz(t, filepath.Join(dir, "data"), planFile)
	html := page(t, f)
	if !strings.Contains(html, "floor plan JSON is invalid") || !strings.Contains(html, "hx-post") {
		t.Error("a broken plan should show its error and still offer an upload")
	}
}

func TestValidAIN(t *testing.T) {
	for s, want := range map[string]bool{
		"116570593149":          true,
		"13077-0012345-1":       true,
		"grp303E4F-3F":          true,
		"":                      false,
		"11657 0593149":         false,
		"a/b":                   false,
		strings.Repeat("1", 65): false,
	} {
		if got := validAIN(s); got != want {
			t.Errorf("validAIN(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestPlugMarkerShowsPowerAndTemperature(t *testing.T) {
	w, c := 42.0, 21.5
	m := newMarker("1", "", map[string]fritzbox.Device{"1": {AIN: "1", Name: "Desk", Present: true, PowerW: &w, TempC: &c}})
	if m.Value != "42" || m.Unit != "W" || m.Temp != "21.5 °C" {
		t.Fatalf("got %q %q %q", m.Value, m.Unit, m.Temp)
	}
	m = newMarker("1", "", map[string]fritzbox.Device{"1": {AIN: "1", Present: true, TempC: &c}})
	if m.Value != "21.5" || m.Temp != "" {
		t.Fatalf("thermometer: got %q %q", m.Value, m.Temp)
	}
}
