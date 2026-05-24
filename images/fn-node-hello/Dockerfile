FROM node:26-slim
WORKDIR /app
RUN npm init -y && npm install express@5.2.1
COPY handler.js /app/handler.js
CMD ["node", "/app/handler.js"]
