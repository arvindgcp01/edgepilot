# Collector fixtures

Output samples for the parsers in `parse.go`.

**These are representative samples, not captures from machines in this
repository's CI.** They reproduce the exact shapes Apple's tools emit — verified
against Apple's documented output and against the field names `system_profiler`
uses — because CI runs Linux and there is no Mac here to capture from. Treat them
as the contract the parsers are written against.

When a real Mac is available, replacing a fixture with genuine output is the
right move; the tests assert on parsed values, so a faithful capture will pass
unchanged.

| File | What it represents | Why it is here |
|---|---|---|
| `hardware_m2pro_macmini.json` | Mac mini, Apple M2 Pro | The common case. `number_processors` is the string `"proc 12:8:4"`. |
| `hardware_m2ultra_macstudio.json` | Mac Studio, Apple M2 Ultra | The other supported model, and a much larger core count. |
| `hardware_intel_macmini.json` | Mac mini (2018), Intel Core i7 | `number_processors` is an **integer**, and the chip is under `cpu_type` rather than `chip_type`. Code that assumes the Apple silicon shape reports zero cores here. |
| `hardware_no_serial.json` | A machine reporting an empty serial | Enrollment is idempotent on the serial number, so this has to fail loudly rather than enroll a device that duplicates itself on every retry. |
| `sw_vers_sonoma.txt` | `sw_vers` on macOS 14.5 | Tab-separated, which trips a naive split on spaces. |
| `sw_vers_sequoia.txt` | `sw_vers` on macOS 15.1 | Confirms the parser is not pinned to one version's formatting. |
| `boottime.txt` | `sysctl -n kern.boottime` | The seconds field is what gets parsed; the trailing date is localised and must be ignored. |
