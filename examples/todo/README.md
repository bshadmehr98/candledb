# Todo example

A small but complete app: accounts, a private todo list per user, live
updates across tabs, and optimistic writes. It consists of one HTML file and
one rules file.

From the repository root:

```sh
make build sdk                                   # the server binary and the JS client
mkdir -p candledb-data
cp examples/todo/candledb.rules candledb-data/   # the server loads <data-dir>/candledb.rules
./bin/candledb serve                             # listens on :8090

# in a second terminal: any static file server, started at the repository root
python3 -m http.server 5173
```

Open <http://localhost:5173/examples/todo/> in two browser windows, create an
account, and add todos in one window while watching the other.

Things to try:

- Stop the server and add a todo. It appears greyed out, then disappears when
  the write fails. Start the server again: the list reconnects by itself.
- Edit `candledb-data/candledb.rules` (for example lower the title limit to
  10 characters), run `pkill -HUP -x candledb`, and add a long todo.
  The server rejects it without a restart.
- Run `make browser-test` from the repository root. It does all of the
  above in three browsers; the script is
  [`test/browser/todo.spec.js`](../../test/browser/todo.spec.js).
- Read `index.html`. The whole app logic is about 80 lines of JavaScript.
