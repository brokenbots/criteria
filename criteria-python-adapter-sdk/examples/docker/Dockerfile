FROM python:3.12-slim AS builder

WORKDIR /app
COPY pyproject.toml ./
RUN pip install --no-cache-dir build && python -m build --wheel

FROM python:3.12-slim
WORKDIR /app
COPY --from=builder /app/dist/*.whl ./
RUN pip install --no-cache-dir *.whl

ENV PYTHONUNBUFFERED=1
ENV CRITERIA_REMOTE_HOST="wss://criteria.example.com:7778"
ENV CRITERIA_REMOTE_TOKEN=""

CMD ["python", "-m", "criteria_adapter"]
