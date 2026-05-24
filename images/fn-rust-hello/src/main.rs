use std::io::Write;
use std::net::TcpListener;

fn main() {
    let listener = TcpListener::bind("0.0.0.0:8080").unwrap();
    for stream in listener.incoming() {
        if let Ok(mut stream) = stream {
            let response = "HTTP/1.1 200 OK\r\nContent-Length: 17\r\n\r\nHello from Rust!";
            stream.write_all(response.as_bytes()).ok();
        }
    }
}
