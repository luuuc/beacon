# Bundler auto-requires the file whose name matches the gem name with
# hyphens kept or converted to slashes — for a gem named "beacon-client"
# that's `require "beacon-client"` first, `require "beacon/client"`
# second. Before this shim existed, the second path accidentally worked
# (there's a `lib/beacon/client.rb` containing the `Beacon::Client`
# class), but it only loaded the class — not the top-level `lib/beacon.rb`
# that defines the module-level API (`Beacon.track`, `Beacon.configure`,
# `Beacon.flush`, …), the Railtie, and the `at_exit` drain hook.
#
# With this file in place, Bundler's default require resolves to
# `lib/beacon-client.rb` first and the real entry point loads as intended.
require "beacon"
