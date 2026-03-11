.PHONY: windows linux test clean

windows:
	GOOS=windows GOARCH=amd64 go build -tags "desktop production" \
		-ldflags="-s -w -H windowsgui" -o build/bin/vintecinco_dl.exe .

linux:
	GOOS=linux GOARCH=amd64 go build -tags "webkit2_41 desktop production" \
		-ldflags="-s -w" -o build/bin/vintecinco_dl .

test:
	go test ./... -count=1

clean:
	rm -rf build/bin/
