# mailer is a library, not a program. Several of the canonical targets
# therefore have no meaningful work to do; they are kept, and made explicit
# about why they are empty, so the command interface stays identical across
# repos and `make <anything>` never fails with "no rule to make target".

.PHONY: build test race cover lint run release clean docker-build deploy logs

# There is no binary to produce. Compiling the package is still the useful
# check, so build does that.
build:
	go build ./...

test:
	go vet ./...
	go test ./...

# The rate limiter and MemorySender carry shared mutable state, so the race
# detector earns its keep here. Run before tagging a release.
race:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	@echo "detail: go tool cover -html=coverage.out"

# gofmt -l lists files needing formatting; the || exit 1 turns that list into
# a failure, which `gofmt -l` alone does not do.
lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi
	go vet ./...

# No command to run. The example is the closest thing to a demo, and it needs
# a live SMTP server, so it is a manual step rather than a target.
run:
	@echo "mailer is a library; there is nothing to run."
	@echo "See README.md for a usage example, or run 'make test'."

# No artifacts to cross-compile: consumers build for their own targets. What
# a release does need is a green race run and a version tag.
release: lint race
	@echo "Tests pass. Tag with:  git tag vX.Y.Z && git push origin vX.Y.Z"
	@echo "Consumers then:        go get github.com/hammondus/mailer@vX.Y.Z"

clean:
	rm -f coverage.out

docker-build:
	@echo "mailer has no container image; it is imported, not deployed."

deploy:
	@echo "mailer has no deployment. Tag a version (see 'make release'),"
	@echo "then update the consuming project's go.mod."

logs:
	@echo "mailer has no running service and therefore no logs."
