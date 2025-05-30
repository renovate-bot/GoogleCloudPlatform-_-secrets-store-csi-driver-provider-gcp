# Copyright 2025 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

VETTERS = "asmdecl,assign,atomic,bools,buildtag,cgocall,composites,copylocks,errorsas,httpresponse,loopclosure,lostcancel,nilfunc,printf,shift,stdmethods,structtag,tests,unmarshal,unreachable,unsafeptr,unusedresult"
GOFMT_FILES = $(shell go list -f '{{.Dir}}' ./...)

fmtcheck:
	@command -v goimports > /dev/null 2>&1 || (cd tools && go install golang.org/x/tools/cmd/goimports && cd ..)
	@CHANGES="$$(goimports -d $(GOFMT_FILES))"; \
		if [ -n "$${CHANGES}" ]; then \
			echo "Unformatted (run goimports -w .):\n\n$${CHANGES}\n\n"; \
			exit 1; \
		fi
	@# Annoyingly, goimports does not support the simplify flag.
	@CHANGES="$$(gofmt -s -d $(GOFMT_FILES))"; \
		if [ -n "$${CHANGES}" ]; then \
			echo "Unformatted (run gofmt -s -w .):\n\n$${CHANGES}\n\n"; \
			exit 1; \
		fi
.PHONY: fmtcheck

spellcheck:
	@command -v misspell > /dev/null 2>&1 || (cd tools && go install github.com/client9/misspell/cmd/misspell && cd ..)
	@misspell -error -source="text" **/*
.PHONY: spellcheck

staticcheck:
	@command -v staticcheck > /dev/null 2>&1 || (cd tools && go install honnef.co/go/tools/cmd/staticcheck && cd ..)
	@staticcheck -checks="all" -tests $(GOFMT_FILES)
.PHONY: staticcheck

licensescsv:
	@command -v go-licenses > /dev/null 2>&1 || (cd tools && go install github.com/google/go-licenses@latest && cd ..)
	@go-licenses csv . | sort > licenses.csv
.PHONY: licensescsv

licensescheck:
	@command -v go-licenses > /dev/null 2>&1 || (cd tools && go install github.com/google/go-licenses@latest && cd ..)
	@go-licenses check . 
.PHONY: licensescheck

licensessave:
	@command -v go-licenses > /dev/null 2>&1 || (cd tools && go install github.com/google/go-licenses@latest && cd ..)
	@go-licenses save . --save_path=licenses
.PHONY: licensessave

gosec:
	@command -v gosec > /dev/null 2>&1 || (cd tools && go install github.com/securego/gosec/v2/cmd/gosec && cd ..)
	# Disable "G307: Deferring a method which returns an error" for noisy alerts on common patterns.
	@gosec -exclude=G307 ./...
.PHONY: gosec

test:
	@go test \
		-count=1 \
		-short \
		-timeout=5m \
		-vet="${VETTERS}" \
		./...
.PHONY: test

test-acc:
	@go test \
		-count=1 \
		-race \
		-timeout=10m \
		-vet="${VETTERS}" \
		./...
.PHONY: test-acc
