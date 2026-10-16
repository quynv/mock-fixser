FROM golang:1.26 AS gen
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY spec/ spec/
COPY fix/gen.go fix/
COPY cmd/webdict/ cmd/webdict/
RUN mkdir -p /out && go generate ./fix && go run ./cmd/webdict -spec spec/FIX44.xml -o /out/dict.json

FROM node:22 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
COPY --from=gen /out/dict.json src/fix/dict.json
RUN npm run build

FROM gen AS build
COPY . .
COPY --from=web /web/dist/ web/dist/
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
