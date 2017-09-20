FROM sergef/docker-library-golang:1.9

ENV HOST_IP 0.0.0.0
ENV CONSUL_HOST consul:8500

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["tini", "--", "/entrypoint.sh"]
