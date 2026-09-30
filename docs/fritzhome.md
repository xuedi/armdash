# FritzHome overview

The FritzHome overview is the page of the present moment: what draws power, what is switched on,
which door or window is open, how warm the rooms are and what needs a look. History lives on the
chart pages next to it; nothing on the overview is a time series.

The page reads the box through the same cached poll that feeds `/metrics`, so opening it adds no
load on the box. While it is open, htmx swaps its body in again every poll interval. A hidden tab
does not poll.

## Sections

Every section appears only when a device for it exists. A box without door sensors shows no empty
"Doors and windows" box.

Tiles sit in Bulma's auto-fit grid: as many columns as have room at 9rem or more each, stretched
across the row. That is up to six across on a desktop and two on a phone; a seventh tile starts a
second row. Every tile and headline card fills its cell, so a row stays one height when one of them
carries an extra line.

Each headline card has a grey note under its value, so none of them is shorter than the rest:

| Card | Note |
|---|---|
| Power now | how many plugs, or the meter it comes from |
| Energy today | whole flat or plugs, since midnight |
| Doors and windows | how long since the last door or window changed, or which are open |
| Indoors | the humidity |
| Attention | the battery that runs out first, the next thing likely to need a look |

| Section | Shows |
|---|---|
| Headline row | power now, energy today, doors and windows, indoor temperature and humidity, the attention count |
| Needs attention | a warning listing each issue, shown only when there is one |
| REST API gaps | readings AHA has and the REST API lacks, only while the REST API is in use, see [fritzbox-metrics.md](fritzbox-metrics.md#aha-or-the-rest-api) |
| Where the power goes | one bar split by device, with a table of every plug, its watts and its share |
| Switches and lights | a tile per plug, bulb or other switch: on or off, watts or brightness |
| Doors and windows | a tile per contact, open ones first, with the time of the last change |
| Climate | a tile per sensor and thermostat, then one per plug with a thermometer |
| Devices | name, model, firmware, whether the box reaches it, battery |

## The power bar

A bar rather than a pie. A handful of plugs with very different draws is exactly the case where
slices are hard to compare and a zero-watt plug disappears. The bar fits a phone width, and the
table under it gives the numbers and serves as its legend.

- **Each plug keeps its colour.** Colours are assigned by AIN, not by rank, so a plug that
  overtakes another does not repaint. The palette has six colours that stay distinguishable for
  colour-blind readers, checked against Bulma's light and dark surfaces. A seventh plug joins a
  grey "Other" rather than reuse a colour. Colour is never the only cue: every segment is named in
  the table.
- **The whole is the meter when there is one.** A FRITZ!Smart Energy 250 on the house meter,
  with the meter's PIN entered in the box, reports the flat's power. The bar then spans that, and the
  gap between the flat and its plugs becomes a grey "Not metered" segment. Without a meter power
  reading the whole is the sum of the plugs.
- A plug drawing nothing stays in the table and is left out of the bar.

## Energy today

The increase of each device's energy counter since local midnight, one Prometheus query. With a
meter the figure is the meter's, since the plugs are already inside it; without one it is the sum of
the plugs. Without Prometheus, or in the day's first minute, the card says n/a.

## Climate

A plug has a thermometer too, but it sits next to its own relay and reads warm. Plugs therefore
come after the real sensors, marked "at the plug", and stay out of the indoor range in the headline
row.

A thermostat tile adds its setpoint, "heating" when the setpoint is above the measured temperature,
window-open and boost when active, and the next scheduled change ("16.0 °C from 22:00"). The box's
setpoint sentinels for off and fully open are shown as words, never as temperatures.

## What counts as attention

- a device the box cannot reach
- a battery the box flags as low, or below 20 %
- a door or window open for more than an hour

## Devices listed twice by the box

HAN-FUN and Zigbee devices appear twice in the box's list: the device itself, with its battery,
and a unit (`<AIN>-1`) with the readings. armdash merges each unit with its device into one entry:
the unit's readings with the device's name, battery and firmware. The unit keeps its own AIN,
because that is the label its history was recorded under, and the bare device entry is dropped.

## Door and window contacts

A contact is a unit of HAN-FUN type 513 (door) or 514 (window) with an alert state. The same alert
element carries other meanings on other devices, blinds for instance, so the unit type decides, not
the element's presence.

## Metrics

The same reading adds `fritz_contact_open` and `fritz_level_percent` to `/metrics`, and
`fritz_battery_percent` now covers every battery device, not only thermostats. See
[fritzbox-metrics.md](fritzbox-metrics.md).
