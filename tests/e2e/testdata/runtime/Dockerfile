FROM golang:1.23 AS fixture-builder

COPY fixturecheck.go /src/fixturecheck.go
RUN CGO_ENABLED=0 go build -trimpath -o /fixturecheck /src/fixturecheck.go

FROM golang:1.23

COPY codex /usr/local/bin/codex
COPY --from=fixture-builder /fixturecheck /usr/local/bin/pf01-fixturecheck
RUN chmod 0755 /usr/local/bin/codex
