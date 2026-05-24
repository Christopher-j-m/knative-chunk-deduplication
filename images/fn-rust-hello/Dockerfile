FROM rust:1.95-slim AS build
WORKDIR /app
COPY Cargo.toml ./
COPY src/ ./src/
RUN cargo build --release

FROM debian:trixie-slim
COPY --from=build /app/target/release/handler /handler
CMD ["/handler"]
