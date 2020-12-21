FROM golang as builder

COPY . /app
WORKDIR /app

RUN CGO_ENABLED=0 go build -o /app/testapp

FROM scratch

COPY --from=builder /app/testapp /testapp

EXPOSE 8080
ENTRYPOINT [ "/testapp" ]