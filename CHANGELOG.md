# Changelog

All notable changes to this project will be documented in this file.
This project adheres to Semantic Versioning.

All changes mention the author, unless contributed by me (@derekparker).

## [0.7.0-alpha] 2015-08-14

### Added

- New command: 'list' (alias: 'ls'). Allows you to list the source code of either your current location, or a location that you describe via: file:line, line number (in current file), +/- offset or /regexp/. (@joeshaw)
- Added Travis-CI for continuous integration. Works for now, will eventually change.
- Ability to connect to headless server. When running Delve in headless mode (used previously only for editor integration), you now have the opportunity to connect to it from the command line with `dlv connect [addr]`. This will allow you to (insecurely) remotely debug an application. (@tylerb)
- Support for printing complex numeric types. (@ebfe)

### Changed

- Deprecate 'run' subcommand in favor of 'debug'. The 'run' subcommand now simply prints a warning, instructing the user to use the 'debug' command instead.
- All 'info' subcommands have been promoted to the top level. You can now simply run 'funcs', or 'sources' instead of 'info funcs', etc...
- Any command taking a location expression (i.e. break/trace/list) now support an updated linespec implementation. This allows you to describe the location you would like a breakpoint (etc..) set at in a more convienant way (@aarzilli).

### Fixed

- Improved support for CGO. (@aarzilli)
- Support for upcoming Go 1.5.
- Improve handling of soft signals on Darwin.
- EvalVariable evaluates package variables. (@aarzilli)
- Restart command now preserves breakpoints previously set.
- Track recurse level when eval'ing slices/arrays. (@aarzilli)
- Fix bad format string in cmd/dlv. (@moshee)
