#!/usr/bin/env node
// WebSocket 流式会话演示：控制面代理到用户实例。
// 用法: node scripts/ws-demo.js <ws-url-with-token>
const url = process.argv[2];
if (!url) {
  console.error("用法: node scripts/ws-demo.js ws://127.0.0.1:8080/v1/users/u-demo/session?token=...");
  process.exit(1);
}

const ws = new WebSocket(url);
const timer = setTimeout(() => {
  console.error("WS 超时");
  process.exit(1);
}, 10000);

ws.onopen = () => {
  ws.send(JSON.stringify({ type: "chat", message: "用 WebSocket 流式发一条消息" }));
};

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  if (msg.type === "delta") {
    process.stdout.write(msg.text);
  } else if (msg.type === "done") {
    clearTimeout(timer);
    console.log(`\n[WS done] message_index=${msg.message_index} model=${msg.model} mock=${msg.mock}`);
    ws.close();
    process.exit(0);
  } else if (msg.type === "error") {
    console.error("WS error:", msg.message);
    process.exit(1);
  }
};
