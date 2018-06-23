# Set to true to disable fancy / colored output
NON_INTERACTIVE ?=

# Set an output prefix, which is the local directory if not specified
PREFIX ?= $(shell pwd)

# If true, turn on verbose output
VERBOSE ?=

# Honor vebose
VERBOSE_GO :=
ifeq ($(VERBOSE),true)
	VERBOSE_GO := -v
endif

# Fancy output if interactive
ifndef NON_INTERACTIVE
    NC := \033[0m
    GREEN := \033[1;32m
    ORANGE := \033[1;33m
    BLUE := \033[1;34m
    RED := \033[1;31m
endif

define title
	@echo "$(GREEN)--------------------------------------------------------------------------------"
	@printf "$(GREEN)%*s\n" $$(( ( $(shell echo "🐳 - $(1) - 🐳" | wc -c ) + 80 ) / 2 )) "🐳 - $(1) - 🐳"
	@echo "$(GREEN)--------------------------------------------------------------------------------$(ORANGE)"
endef

# Used to populate version variable in main package.
VERSION=$(shell git describe --match 'v[0-9]*' --dirty='.m' --always)

# Allow turning off function inlining and variable registerization
ifeq (${DISABLE_OPTIMIZATION},true)
	GO_GCFLAGS=-gcflags "-N -l"
	VERSION:="$(VERSION)-noopt"
endif

GO_LDFLAGS=-ldflags "-X `go list ./version`.Version=$(VERSION)"

.PHONY: all build-clean clean dep-validate fmt vet lint meta test test-full build binaries
.DEFAULT: all
all: fmt vet lint meta test test-full build binaries

AUTHORS: .mailmap .git/HEAD
	 git log --format='%aN <%aE>' | sort -fu > $@

# This only needs to be generated by hand when cutting full releases.
version/version.go:
	./version/version.sh > $@

# Resolving binary dependencies for specific targets
GOLINT=$(shell which golint || echo '')
DEP=$(shell which dep || echo '')
GOMETA=$(shell which gometalinter.v2 || echo '')

############################
# Building
############################

$(PREFIX)/bin/%: ./cmd/%/main.go $(shell find . -type f -name '*.go')
	$(call title, $@)
	go build $(VERBOSE_GO) -tags "${DOCKER_BUILDTAGS}" -o $@$(call extension,$(GOOS)) ${GO_LDFLAGS} ${GO_GCFLAGS} $<

docs/spec/api.md: docs/spec/api.md.tmpl ${PREFIX}/bin/registry-api-descriptor-template
	./bin/registry-api-descriptor-template $< > $@

binaries: $(patsubst ./cmd/%/main.go,$(PREFIX)/bin/%,$(wildcard ./cmd/*/main.go))

build-clean:
	$(call title, $@)
	rm -Rf "$(PREFIX)/bin"

clean: build-clean

############################
# Testing and validation
############################

# XXX Shadow doesn't pass right now
#	go vet $(VERBOSE_GO) -tags "${DOCKER_BUILDTAGS}" -shadow=true ./...
vet: build
	$(call title, $@)
	go vet $(VERBOSE_GO) -tags "${DOCKER_BUILDTAGS}" ./...

fmt:
	$(call title, $@)
	test -z "$$(gofmt -s -l . 2>&1 | grep -v ^vendor/ | tee /dev/stderr)" || \
		(echo >&2 "+ please format Go code with 'gofmt -s -w'" && false)

lint:
	$(call title, $@)
	$(if $(GOLINT), , \
		$(error Please install golint: `go get -u github.com/golang/lint/golint`))
	test -z "$$($(GOLINT) ./... 2>&1 | grep -v ^vendor/ | tee /dev/stderr)"

meta:
	$(call title, $@)
	$(if $(GOMETA), , \
		$(error Please install gometa: `go get -u gopkg.in/alecthomas/gometalinter.v2`))
	test -z "$$($(GOMETA) --config .gometalinter.json ./... 2>&1 | tee /dev/stderr)"

build:
	$(call title, $@)
	go build $(VERBOSE_GO) -tags "${DOCKER_BUILDTAGS}" -v ${GO_LDFLAGS} ./...

test:
	$(call title, $@)
	go test $(VERBOSE_GO) -tags "${DOCKER_BUILDTAGS}" -cpu=1 -parallel 1 -vet=off -test.short ./...

test-full:
	$(call title, $@)
	go test $(VERBOSE_GO) -tags "${DOCKER_BUILDTAGS}" -cpu=1,2,4 -parallel 4 -vet=off -race -bench . ./...

############################
# Dependencies helpers
############################

dep-validate:
	$(call title, $@)
	$(if $(DEP), , \
		$(error Please install dep: go get github.com/golang/dep))
	$(DEP) ensure $(VERBOSE_GO)
	test -z "$$(git status --porcelain 2>&1 | tee /dev/stderr)" || \
		(echo >&2 "+ inconsistent dependencies! you need to run 'dep ensure' and commit changes" && false)

dep-status:
	$(call title, $@)
	$(DEP) status $(VERBOSE_GO)

dep-update:
	$(call title, $@)
	$(DEP) ensure -update $(VERBOSE_GO)
