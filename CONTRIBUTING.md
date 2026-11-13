# Contributing

Apache 2.0, with the **Developer Certificate of Origin** (DCO) like [Kevaso core](https://github.com/dlebee/kevaso):
sign off every commit with `git commit -s`.

- `make vet test` before pushing. The `juicefs` command is faked (`juicefs/fakerunner_test.go`);
  everything the provider runs goes through the `Runner` interface (`juicefs/runner.go`), so a new
  command is a new method there and in the fake.
- Credentials only ever go in the `juicefs` command's environment, never its arguments, the config
  or a log line; `TestFormatsOnlyOnce` checks the arguments.
- Until core publishes the plugin protocol's node side (`sdk/kplugin`'s `Node` service), this module
  builds in a Go workspace with core next to it: a `go.work` (never committed) that uses `.` and
  core's three modules.
