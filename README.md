# boltflow

Move MX Keys + MX Master between hosts **together**, locally over the Logi
Bolt receiver — no network involved. Companion to `~/projects/ddc/lgddc`
(monitor input switching) for the one-button KVM setup.

## How follow works

The Easy-Switch keys cannot be intercepted (not divertable, capability flags
0x04). But when one is pressed, the device announces the change **to the host
it is leaving** as an unsolicited HID++ event on its Change Host feature
(0x1814) before jumping:

```
11 <devIdx> <idx1814> 00 <p0> <targetHost>
```

This event is not in Logitech's public HID++ docs — it's what Logi Options+
uses for its own follow behavior. `boltflow sniff` makes it visible (that's
how it was found here, on an MX Keys S over a Bolt receiver). `run` listens
for it from every device and moves all the others to the same host. The keys
stay completely native; nothing is diverted or remapped.

Follow is symmetric: switching the mouse via its bottom button drags the
keyboard along too.

## Usage

```
boltflow list           # devices, host channels, controls, feature indexes
boltflow switch <1-3>   # move all devices to a host now
boltflow run            # follow agent: easy-switch moves everything
boltflow sniff          # dump raw HID++ traffic (debugging)
```

A host can only command devices currently connected to *its* receiver —
that's physics: commands travel over the active radio link. So the follow
agent must run on **every** host you switch away from (build with
`go build`; cgo/hidapi, works on macOS and Windows). On hosts without the
agent, Easy-Switch just behaves stock (that device only).

## Building

`go build` — cgo is required (hidapi). On Linux install `libudev-dev` first;
at runtime the hidraw device needs permissions (udev rule or root). CI builds
macOS (arm64/amd64/universal) and Linux amd64 on push; tagging `v*` attaches
the binaries to a GitHub release.

## Protocol notes

HID++ 2.0 via the Bolt receiver's vendor interface (usage page 0xFF00):
feature 0x1814 (Change Host) for moves and the departure event, 0x0005 for
name/type, 0x1B04 (Reprog Controls v4) only for the `list` capability dump.
Device discovery pings indexes 1–6 instead of touching receiver registers.
