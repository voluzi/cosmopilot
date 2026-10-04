FROM ghcr.io/voluzi/cosmosigner:edge-pkcs11 AS signer

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends softhsm2 opensc curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=signer /bin/cosmosigner /usr/local/bin/cosmosigner
COPY provision-token.sh /usr/local/bin/provision-token
RUN chmod 755 /usr/local/bin/provision-token
USER 1000:1000
ENTRYPOINT ["cosmosigner"]
