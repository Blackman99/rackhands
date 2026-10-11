# Contributing

1. Read [GOAL.md](GOAL.md). Features outside it will be declined. New dangerous operations need a new class (off by default) and must go through `bmc.Plan`.
2. `gofmt` clean, `go vet ./...` and `go test -race ./...` must pass.
3. Vendor behaviour is captured as mockups. To add or fix a vendor quirk, extend `testdata/mockups/make_mockups.py`, regenerate (`python3 testdata/mockups/make_mockups.py`), and add a unit test in `internal/bmc/redfish_test.go`. Anonymised captures from real BMCs are very welcome.
4. `test/integration.sh` runs the real binary against the DMTF Redfish-Mockup-Server and OpenIPMI `ipmi_sim` in CI. Add a check there when behaviour depends on a real server or the IPMI wire protocol.
5. Never weaken the safety model in a PR that does something else.
