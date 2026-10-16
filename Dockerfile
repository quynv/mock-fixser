FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY spec/ spec/
COPY fix/gen.go fix/
RUN go generate ./fix
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mockfix ./cmd/mockfix

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/mockfix /app/mockfix
COPY spec/ /app/spec/
# Mount configs/ and data/ (replay files) as volumes. Port 9877 (monitor) stays inside the container.
VOLUME ["/app/configs", "/app/data"]
EXPOSE 9876 8080
ENTRYPOINT ["/app/mockfix"]
CMD ["--config", "/app/configs/mock.yaml"]
