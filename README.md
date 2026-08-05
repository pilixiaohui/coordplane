# CoordPlane

CoordPlane is a local-first daemon for coordinating isolated CLI agents against a shared Git project. The v1 product contract is defined only by these five documents:

- [Product scope](need/README.md)
- [Core objects and state](need/core.md)
- [Runtime and isolation](need/runtime.md)
- [Git truth and integration](need/git.md)
- [Acceptance gates](need/acceptance.md)

The current implementation is being delivered in vertical stages. The active surface uses file-backed SQLite, a local operator Unix socket, the `coordplane` Boss CLI, and the `coordlink` per-Run CLI. Removed platform APIs and schemas are intentionally not compatibility contracts; existing development databases from the pre-v1 model must use a new `data_dir`.

Build and verify the available stage with:

```bash
make build
make test
make race
make vet
```
