install_binaries:
	go install github.com/golang/mock/mockgen@v1.6.0

generate_mocks:
	go generate ./...

install: install_binaries generate_mocks

clean:
	@rm -rf ./dist ./mocks

test:
	@go test ./...

d: dev
i: install
c: clean

# watch / develop
dev_pipeline: test
watch:
	@watchexec -cr -f "*.go" -- make dev_pipeline
dev: watch

