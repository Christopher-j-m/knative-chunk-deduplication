FROM eclipse-temurin:25-jdk-alpine AS build
WORKDIR /app
COPY Handler.java .
RUN javac Handler.java && jar --create --file /handler.jar --main-class Handler Handler.class

FROM eclipse-temurin:25-jre-alpine
COPY --from=build /handler.jar /app/handler.jar
CMD ["java", "-jar", "/app/handler.jar"]
