import assert from "node:assert/strict";
import { createServer } from "node:http";
import { describe, test } from "node:test";

import { createClient } from "../dist/index.js";
import { waitFor } from "./helpers.js";

// A connection can die without either side being told: a laptop sleeps, a
// NAT box forgets the flow. The server's keep-alives exist so the client
// can notice; this test plays a server that goes silent after "ready".
describe("stream watchdog", () => {
  test("a stream that goes silent is reopened", async () => {
    let connections = 0;
    const silent = createServer((req, res) => {
      if (req.url === "/v1/listen") {
        connections++;
        res.writeHead(200, { "Content-Type": "text/event-stream" });
        res.write(`data: {"type":"ready","session":"s${connections}"}\n\n`);
        return; // and then nothing, not even a keep-alive
      }
      res.writeHead(204).end(); // accept target registrations
    });
    await new Promise((resolve) => silent.listen(0, "127.0.0.1", resolve));
    const url = `http://127.0.0.1:${silent.address().port}`;

    const db = createClient({ url, reconnect: { idleTimeoutMs: 150, minDelayMs: 10, maxDelayMs: 20 } });
    const stop = db.doc("a/b").onSnapshot(() => {}, () => {});
    try {
      await waitFor(() => connections >= 3, { message: "the client did not reconnect after the stream went silent" });
    } finally {
      stop();
      db.close();
      silent.closeAllConnections();
      await new Promise((resolve) => silent.close(resolve));
    }
    assert.ok(connections >= 3);
  });
});
