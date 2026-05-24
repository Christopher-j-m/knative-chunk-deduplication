import java.io.OutputStream;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;

public class Handler {
    public static void main(String[] args) throws Exception {
        try (ServerSocket server = new ServerSocket(8080)) {
            while (true) {
                try (Socket socket = server.accept()) {
                    byte[] body = "Hello from Java!".getBytes(StandardCharsets.UTF_8);
                    String headers = "HTTP/1.1 200 OK\r\nContent-Length: " + body.length + "\r\n\r\n";
                    OutputStream out = socket.getOutputStream();
                    out.write(headers.getBytes(StandardCharsets.UTF_8));
                    out.write(body);
                }
            }
        }
    }
}
