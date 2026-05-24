FROM python:3.14-slim
RUN pip install --no-cache-dir fastapi==0.136.1 uvicorn==0.46.0 httpx==0.28.1
COPY handler.py /app/handler.py
CMD ["uvicorn", "handler:app", "--host", "0.0.0.0", "--port", "8080"]
