# Run the agent in a container while still measuring the host:
#
#   docker run -d --name towamori-agent --restart unless-stopped \
#     --pid host --network host --uts host \
#     -v /:/host:ro,rslave -e TOWAMORI_HOST_ROOT=/host \
#     --env-file /etc/towamori/agent.env \
#     towamori-agent
#
# Build from the repository root after `make dist`, or with any linux binary
# named towamori-agent in the build context.
FROM alpine:3.22
COPY towamori-agent /usr/local/bin/towamori-agent
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/towamori-agent"]
