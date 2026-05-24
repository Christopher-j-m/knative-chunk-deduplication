const fastify = require("fastify")();
fastify.get("/", async () => ({ status: "ok" }));
fastify.listen({ port: 8080, host: "0.0.0.0" });
