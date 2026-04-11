Gem::Specification.new do |s|
  s.name        = "beacon-client"
  s.version     = "0.2.1"
  s.summary     = "Ruby client for Beacon — the small observability accessory"
  s.description = "Captures perf, errors, and outcomes from a Rack/Rails app and ships them to a Beacon server."
  s.authors     = ["Luc B. Perussault-Diallo"]
  # O'Saasy license — MIT-style for all non-SaaS-compete use. RubyGems
  # requires an SPDX identifier or the literal "Nonstandard" for any
  # license that isn't on the SPDX list. See LICENSE at the repo root
  # or https://osaasy.dev for the canonical text.
  s.license     = "Nonstandard"
  s.homepage    = "https://github.com/luuuc/beacon"
  s.required_ruby_version = ">= 3.1"

  s.files = Dir["lib/**/*.rb", "README.md"]
  s.require_paths = ["lib"]

  s.add_development_dependency "minitest", "~> 5.20"
  s.add_development_dependency "rack",     ">= 2.2"
end
