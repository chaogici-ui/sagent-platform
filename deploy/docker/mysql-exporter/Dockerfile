FROM python:3.12-slim
WORKDIR /app
COPY exporter.py .
ENV EXPORTER_PORT=9104
CMD ["python", "-u", "exporter.py"]
