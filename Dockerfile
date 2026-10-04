FROM public.ecr.aws/docker/library/golang:alpine AS builder

ARG TARGETARCH

RUN go env -w CGO_ENABLED=0

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -extldflags '-static -fpic'" -o bsos main.go

FROM public.ecr.aws/docker/library/alpine

RUN apk add --no-cache util-linux e2fsprogs

WORKDIR /app

COPY --from=builder --chmod=755 /app/bsos /app/bsos

ENTRYPOINT ["/app/bsos"]
