FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /simplea2a

FROM scratch
COPY --from=build /simplea2a /simplea2a
ENV DATA=/data
CMD ["/simplea2a"]
