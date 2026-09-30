# Floor plan

The FritzHome floor plan page draws the flat as SVG and puts every FRITZ!Box device on it with its
live reading. The drawing comes from SweetHome3D or from a hand-traced JSON file; the devices are
placed by dragging them on the page.

## Where the plan comes from

Three sources, and the first one present wins:

1. a drawing or a picture **uploaded on the page**, kept in the data directory
2. `AD_SYSTEM_FRITZHOME_FLOORPLAN_FILE`, a SweetHome3D `.sh3d` file or a JSON plan
3. `AD_SYSTEM_FRITZHOME_FLOORPLAN`, a JSON plan inline

The info bar says which one is in use, so an upload silently shadowing the configured file is never
a mystery. The format is picked by **content, not extension**: a file starting with the ZIP
signature is SweetHome3D, anything else is parsed as JSON.

The plan is re-read on every request. An upload, a saved position or a new file on disk shows up on
the next reload without a restart.

A plan that fails to load is shown as an error on the page rather than failing it, so the upload
button stays there to replace it.

## SweetHome3D

A `.sh3d` file is a ZIP archive. Since SweetHome3D 5.3 it carries a `Home.xml` next to the
Java-serialised home, and that XML is the only entry read. Models and textures are never opened, so a
drawing full of them costs no more than a bare one. Older files have no `Home.xml` and are rejected
with a note to re-save them in a current version. `Home.xml` is capped at 16 MB once decompressed,
which is what stops a small archive from inflating into gigabytes.

SweetHome3D coordinates are centimetres with y pointing down, the same orientation as SVG, so they
become SVG user units one to one. The view box is the bounding box of everything drawn plus a margin.
A flat of eight or nine metres comes out the same size as a traced plan, so markers and labels need
no scaling.

What is drawn:

| From the drawing | On the page |
|---|---|
| walls | lines at their own thickness, with square ends that close the corners |
| rooms | filled polygons, the name centred on the room's bounding box plus the offset it was dragged by, as SweetHome3D draws it |
| doors | red, over the wall they interrupt |
| windows | a gap in the wall with a thin pane across it |
| furniture | faint outlines, with the name as a tooltip |

**Doors and windows are told apart by elevation.** SweetHome3D does not record which is which, and
the catalogue names are localised, so an opening that starts at the floor is a door and one that
starts above it is a window.

**An opening is drawn where it meets the wall, not where its footprint is.** A door's footprint
includes the leaf standing proud of the wall. SweetHome3D records which slice of the depth sits
inside the wall, and that slice's centre line is what gets drawn, so a door lands exactly on the wall
it belongs to.

Invisible furniture is skipped. Furniture groups are flattened, their pieces already carry absolute
coordinates.

A drawing needs **rooms** to have fills and labels: in SweetHome3D, *Plan > Create rooms*, then
double-click inside each space. Without them the plan is walls only.

Not drawn: curved walls (drawn straight), any level but the lowest, colours, textures, text labels,
dimension lines and everything 3D.

## JSON

Plain SVG geometry, so a plan can be traced from a sketch by hand:

| Key | Meaning |
|---|---|
| `width`, `height` | the view box, 800 by 500 when left out |
| `outline` | the flat's outer wall, an SVG polygon point string |
| `rooms[]` | `points` polygon plus a `name` and optional `labelX`/`labelY` |
| `walls[]` | interior walls, each an SVG polyline point string |
| `doors[]` | `x1,y1,x2,y2` segments, drawn over the walls they interrupt |
| `devices[]` | `ain` plus `x`/`y`, and an optional `label` overriding the device name |

**Watch the polygon winding.** A room drawn with a concave step can exclude the pocket you meant to
include, which shows up as an unfilled white patch rather than an error. A point-in-polygon check on
a coordinate inside the questionable area is the quickest way to confirm the shape is what you
think.

## Uploading

The **Upload** button in the info bar, shown once logged in, posts the chosen file straight away. It takes a SweetHome3D
drawing or a picture of the flat: an SVG, PNG, JPEG or GIF. What a file is gets decided by its
content, never its name.

The upload is streamed into a temp file in the data directory, never held in memory, and has to
pass before it replaces anything: at most 64 MB, and either a readable SweetHome3D file with at least
one wall or room on its lowest level, or a picture whose size can be read. Only then is it renamed
over `floorplan`, so a reader sees the old plan or the new one, never half of either. The previous
plan is kept as `floorplan.prev`, which makes one bad upload a rename away from undone.

A rejected file answers 422 with a notice saying why, which the page shows above the plan. htmx
fires its swap events on the target rather than on the form, so the target is where the 422 is let
through.

### Pictures

A picture becomes the whole plan, drawn as the background with the devices on top. Only its size
is read on the server, from the image header or from the SVG's `viewBox` (or its `width` and
`height`); the browser does the decoding. The long side is scaled to 1000 units: photos run to
thousands of pixels and hand-made SVGs to hundreds, and a fixed scale keeps markers and labels the
same size on either.

The picture is served back from the system's API with its modification time in the URL, so a new
upload is never hidden by the browser cache. An SVG can carry script. Drawn through `<image>` it
never runs, and for the case where someone opens the URL on its own the response carries a
sandboxing `Content-Security-Policy` and `nosniff`. Pictures are upload only; `floorplan_file`
takes a drawing or JSON.

## Placing devices

Devices are placed by **AIN**, never by name, because names are not unique: a FRITZ!Smart Energy 250
reports as two entries sharing one name, distinguished only by an AIN suffix.

**Edit** turns the markers draggable. Devices the box reports but the plan has not placed wait in a
strip below the flat; drag one onto the plan to place it, or drag a placed one back into the strip
to take it off. **Save** sends every marker above the strip, **Cancel** reloads the page. Nothing is
stored before Save, and the dragging is a few lines of plain script in the page, not a library.

Saved positions go to `positions.json` in the data directory, a map of AIN to `x`/`y` in plan
units. Once that file exists it **replaces** the plan's own device positions entirely, so a device
taken off in edit mode stays off. Labels from a JSON plan still apply. Deleting the file goes back to
the plan's own placement.

A saved position outside the current plan, typically after a drawing was replaced by a picture of
another size, puts that device back in the strip rather than somewhere off-screen where it could
never be dragged back.

## Who can change it

The drawing and the device positions are data rather than settings, see
[configuration.md](configuration.md). They are kept in the system's own data directory: `AD_CORE_DATA_DIR/fritzhome`, or
`/var/lib/armdash/fritzhome` under the packaged unit, whose `StateDirectory=` provides it. With no
data directory the page offers neither button and both endpoints answer 404.

Anyone who can open the dashboard sees the plan; only the logged-in owner can replace it or move
the devices, see [authentication.md](authentication.md). Logged out, the page draws neither button,
and the shell answers a write with 401 before it reaches the system. If the session ends while the
page is open, the upload shows that notice where a rejected file's would go. Every system endpoint
also goes through Go's cross-origin protection, so a page on another site cannot make the owner's
browser post to it.

## Colour

Colour carries meaning: teal for power, indigo for temperature, red for doors and for a device the
box reports as absent. Everything else is a Bulma variable, so the plan follows the light and dark
palettes.
