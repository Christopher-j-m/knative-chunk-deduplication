FROM python:3.14-slim
RUN pip install --no-cache-dir flask==3.1.3
COPY handler.py /app/handler.py
CMD ["python", "/app/handler.py"]
