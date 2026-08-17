FROM python:3.12-alpine

RUN adduser -D -u 1001 user
COPY fake_opencode.py /opt/fern-lifecycle/fake_opencode.py
RUN chmod 0555 /opt/fern-lifecycle/fake_opencode.py \
    && mkdir -p /home/user/workspace /home/user/.local/share/opencode \
    && chown -R user:user /home/user

USER user
WORKDIR /home/user/workspace
EXPOSE 4096
CMD ["python3", "/opt/fern-lifecycle/fake_opencode.py"]
