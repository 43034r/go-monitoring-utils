FROM golang:1.23 as build

WORKDIR /app

COPY go.mod ./
COPY go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o kh .

FROM gcr.io/distroless/base

COPY --from=build /app/kh /kh

USER nonroot:nonroot

CMD ["/kh"]