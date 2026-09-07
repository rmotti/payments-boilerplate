# Repositories PostgreSQL

Implementacoes de persistencia para os casos de uso. Os repositories de pedido e
pagamento usam GORM, transacoes curtas e constraints do schema para garantir
idempotencia e impedir checkouts concorrentes.

O repository de webhooks usa queries geradas por `sqlc` sobre `database/sql`,
porque a forma dessas queries faz parte da garantia: o `ON CONFLICT DO NOTHING`
e o que deduplica uma reentrega, e o inbox e o outbox precisam ser gravados na
mesma transacao. As duas tabelas sao novas, entao nenhuma transacao mistura os
dois estilos de acesso, como pede o ADR 0007.

O polling com lease do relay e as transicoes condicionais do consumer seguem o
mesmo caminho. O consumer grava tudo em uma unica transacao `sqlc`: ele bloqueia
o evento e depois tentativa, pagamento e pedido em ordem fixa, e cada transicao
condicional precisa alterar exatamente uma linha. GORM nao participa dessa
transacao, o que mantem a regra do ADR 0007 sem precisar de uma ponte.

A fronteira entre os dois estilos passa a ser por query, e nao por tabela:
`payments`, `payment_attempts` e `orders` sao escritas por GORM no checkout da
API e por `sqlc` no consumer do worker. Uma mudanca de schema precisa considerar
os dois caminhos.
