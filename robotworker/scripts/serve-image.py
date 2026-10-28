"""Serves a Robot worker's disk image once, over TLS 1.3, to the worker's address alone.

Usage: serve-image.py <worker address> <port> <TLS certificate> <TLS key> <image>

It exists because the image embeds every key of the cluster: the TLS key is
deleted once loaded, and the image once the worker read one whole copy and
closed the connection.
"""

import os
import socket
import ssl
import sys

MAX_REQUEST = 8192
TIMEOUT = 120


def serve_one(tls, image, size):
    request = b""
    while b"\r\n\r\n" not in request:
        chunk = tls.recv(1024)
        if not chunk or len(request) > MAX_REQUEST:
            return False
        request += chunk
    head = request.startswith(b"HEAD /image.raw ")
    if not head and not request.startswith(b"GET /image.raw "):
        tls.sendall(b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        return False
    tls.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n"
                b"Content-Length: %d\r\nConnection: close\r\n\r\n" % size)
    if head:
        return False
    with open(image, "rb") as f:
        while chunk := f.read(1 << 20):
            tls.sendall(chunk)
    return reader_closed(tls)


def reader_closed(tls):
    """Counts a fetch served only once the worker closes after the last byte, since sendall only fills a buffer."""
    try:
        return tls.recv(1) == b""
    except (ssl.SSLZeroReturnError, ssl.SSLEOFError):
        return True


def main():
    worker, port, cert, key, image = sys.argv[1:6]
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_3
    context.load_cert_chain(cert, key)
    os.remove(key)
    size = os.path.getsize(image)
    with socket.create_server(("", int(port))) as listener:
        while True:
            conn, (peer, *_) = listener.accept()
            with conn:
                if peer != worker:
                    continue
                conn.settimeout(TIMEOUT)
                try:
                    with context.wrap_socket(conn, server_side=True) as tls:
                        if serve_one(tls, image, size):
                            break
                except OSError as err:
                    print(f"serve-image: a fetch from {peer} ended early: {err}", file=sys.stderr)
    os.remove(image)
    print(f"serve-image: served the image once to {worker}")


if __name__ == "__main__":
    main()
