/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "check.h"
#include "urlutil.h"

int main(void)
{
    char b[256];
    url_with_time(b, sizeof b, "http://127.0.0.1:8080/v/abc.ts", 742.5);
    CHECK_STR(b, "http://127.0.0.1:8080/v/abc.ts?t=742.5");
    url_with_time(b, sizeof b, "http://127.0.0.1:8080/v/abc.ts?q=1", 10);
    CHECK_STR(b, "http://127.0.0.1:8080/v/abc.ts?q=1&t=10");
    url_with_time(b, sizeof b, "http://h/v.ts?t=5", 20.25);
    CHECK_STR(b, "http://h/v.ts?t=20.25");
    url_with_time(b, sizeof b, "http://h/v.ts?a=1&t=5&b=2", 0.1234);
    CHECK_STR(b, "http://h/v.ts?a=1&b=2&t=0.123");
    url_with_time(b, sizeof b, "http://h/v.ts?a=1&t=5", 3);
    CHECK_STR(b, "http://h/v.ts?a=1&t=3");
    url_with_time(b, sizeof b, "http://h/d/x.ts#frag", -4);
    CHECK_STR(b, "http://h/d/x.ts?t=0#frag");
    url_with_time(b, sizeof b, "http://h/v.ts?tt=1", 7);
    CHECK_STR(b, "http://h/v.ts?tt=1&t=7");
    CHECK(url_is_network("http://x") && url_is_network("HTTPS://x"));
    CHECK(!url_is_network("/mnt/SDCARD/x.ts") && !url_is_network("file:/x.ts"));
    double v = 0;
    const char *hdr = "HTTP/1.1 200 OK\r\nContent-Type: video/mp2t\r\nx-minifin-start:  740.52\r\nConnection: close\r\n\r\n";
    CHECK(http_header_number(hdr, "X-MiniFin-Start", &v) == 1);
    CHECK_NEAR(v, 740.52, 1e-9);
    CHECK(http_header_number(hdr, "X-MiniFin-Other", &v) == 0);
    CHECK(http_header_number("X-MiniFin-Start: nan\r\n", "X-MiniFin-Start", &v) == 0);
    CHECK(http_header_number("X-MiniFin-Start: 12abc\r\n", "X-MiniFin-Start", &v) == 0);
    CHECK(http_header_number("A: 1\r\nB-X-MiniFin-Start: 3\r\n", "X-MiniFin-Start", &v) == 0);
    char host[64], path[256];
    int port;
    CHECK(url_split_http("http://127.0.0.1:8080/v/a.ts?t=1", host, sizeof host, &port, path, sizeof path) == 0);
    CHECK_STR(host, "127.0.0.1");
    CHECK(port == 8080);
    CHECK_STR(path, "/v/a.ts?t=1");
    CHECK(url_split_http("http://example", host, sizeof host, &port, path, sizeof path) == 0 && port == 80);
    CHECK_STR(path, "/");
    CHECK(url_split_http("https://x/y", host, sizeof host, &port, path, sizeof path) < 0);
    CHECK(url_split_http("http://h:99999/", host, sizeof host, &port, path, sizeof path) < 0);
    return CHECK_DONE();
}
