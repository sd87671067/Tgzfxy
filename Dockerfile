FROM python:3.13-slim

RUN apt-get update && apt-get install -y --no-install-recommends util-linux \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app.py forwarder.py maintenance.py ./
CMD ["python", "-u", "app.py"]
