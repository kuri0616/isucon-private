all: app

app: *.go go.mod go.sum
	go build -o app

exportimages: cmd/exportimages/*.go go.mod go.sum
	go build -o exportimages ./cmd/exportimages

.PHONY: all
