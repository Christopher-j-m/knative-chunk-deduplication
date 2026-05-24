FROM python:3.14-slim
RUN pip install --no-cache-dir numpy==2.4.4 scikit-learn==1.8.0 flask==3.1.3
COPY handler.py /app/handler.py
CMD ["python", "/app/handler.py"]
