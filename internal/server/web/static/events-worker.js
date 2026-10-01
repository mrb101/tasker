// One live-update stream shared by every open tasker tab.
// Browsers allow only ~6 HTTP/1.1 connections per host; a stream per tab
// would use them all up and stall page loads.
const ports = new Set();
let es = null;

function open() {
  if (es) return;
  es = new EventSource('/api/events');
  es.addEventListener('change', (e) => ports.forEach((p) => p.postMessage(e.data)));
}

function close() {
  if (es) { es.close(); es = null; }
}

onconnect = (e) => {
  const port = e.ports[0];
  ports.add(port);
  port.onmessage = (m) => {
    if (m.data === 'bye') {
      ports.delete(port);
      if (!ports.size) close();
    }
  };
  port.start();
  open();
};
