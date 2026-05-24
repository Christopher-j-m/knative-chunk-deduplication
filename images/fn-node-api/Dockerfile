FROM node:26-slim
WORKDIR /app
RUN npm init -y && npm install fastify@5.8.5 axios@1.16.0
COPY handler.js /app/handler.js
CMD ["node", "/app/handler.js"]
