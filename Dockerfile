# Start from the official Go image that the Docker community maintains, pulled
# through the Amazon ECR Public Gallery. The tag is the Go version that built
# the release.
FROM public.ecr.aws/docker/library/golang:1.27.3 AS build

# CGO_ENABLED=0 builds the binaries from Go code only. The build does not use
# this image's C compiler or C library.
# GOTOOLCHAIN=local keeps Go on the version in this image. Go then never
# downloads another Go version.
# -mod=readonly keeps Go from changing go.mod or go.sum. If one of the two
# files needs a change, the build stops.
# -buildvcs=false leaves the git commit out of the binaries. A build from a git
# clone and a build from the source archive then give the same files.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS="-mod=readonly -buildvcs=false"

# Download the Go modules that the program uses. Check each one against the
# checksums in go.sum.
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Build the program for each release platform, and for Windows. The command
# uses the release build flags.
# -trimpath leaves this image's directory names out of the binaries.
# -ldflags "-s -w" leaves the debugging data out of the binaries.
COPY . .
RUN mkdir /out && for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
      case $t in windows/*) ext=.exe;; *) ext=;; esac; \
      GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags "-s -w" \
        -o /out/devicefarm-connect_${t%/*}_${t#*/}$ext ./cmd/devicefarm-connect || exit 1; \
    done

# Join the two macOS binaries into one file that runs on Apple silicon and
# Intel Macs. The makefat tool joins them. Go downloads makefat at a fixed
# commit.
RUN go run github.com/randall77/makefat@7ddd0e4 /out/devicefarm-connect_darwin_universal \
      /out/devicefarm-connect_darwin_amd64 /out/devicefarm-connect_darwin_arm64

# Keep the six binaries and nothing else. The --output option of docker build
# then writes only those six files.
FROM scratch
COPY --from=build /out/ /
