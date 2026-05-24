FROM python:3.11-slim
RUN pip install --no-cache-dir \
    torch==2.7.0+cpu \
    --index-url https://download.pytorch.org/whl/cpu && \
    pip install --no-cache-dir flask==3.1.3
COPY handler.py /app/handler.py
EXPOSE 8080
CMD ["python", "/app/handler.py"]
