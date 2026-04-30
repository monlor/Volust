FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/volust ./cmd/volust

FROM alpine:3.20
RUN apk add --no-cache ca-certificates restic rclone rsync
COPY --from=build /out/volust /usr/local/bin/volust
ENTRYPOINT ["volust"]
CMD ["daemon"]
