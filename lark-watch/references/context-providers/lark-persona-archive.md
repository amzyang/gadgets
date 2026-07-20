---
name: lark-persona-archive
provides: 与同事的历史聊天记录——此前的约定、结论、字段/方案的设计由来
when: 需要查证「这事之前聊过什么」——对方提到的术语或方案疑似有前文讨论
cost: fast
---
grep -rih '<关键术语>' ~/.local/share/lark-persona/archive/msgs/ | tail -30
消息为 NDJSON（含 from/t/text，按会话 cid、月份分文件；cid 对应的会话名查
archive/chats.ndjson），多个术语分别 grep；找不到即算无据，不做全文通读。
