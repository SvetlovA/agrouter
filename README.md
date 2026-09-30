# agrouter

## Install

```sh
go install github.com/SvetlovA/agrouter/cmd/agrouter@latest
agrouter --version
```

## Releasing

1. Merge the release changes into `master`.
2. Tag a commit on `master` with a `v0.x.y` or `v1.x.y` semver tag (optionally with a lowercase pre-release suffix, e.g. `v0.1.0-rc.1`) and push the tag:

   ```sh
   git tag v0.1.0
   git push origin v0.1.0
   ```

3. The `release` workflow validates the tag, re-runs the tests, creates a GitHub Release with generated notes (pre-release for suffixed tags) and warms the Go module proxy. The repository must be public for the proxy to fetch the module.
4. Install the release:

   ```sh
   go install github.com/SvetlovA/agrouter/cmd/agrouter@v0.1.0
   ```
