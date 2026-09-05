# ADR 0001: limites do produto

- Status: aceito
- Data: 2026-09-05

## Contexto

O projeto nasce como um componente open source reutilizável para pagamentos. A
expressão pode descrever desde um exemplo de checkout até um gateway completo,
portanto seus limites precisam ser registrados antes da implementação.

## Decisão

Construiremos uma aplicação de referência que orquestra um provedor certificado
e mantém o estado de negócio necessário à aplicação.

A primeira versão implementará pagamento único em BRL usando checkout hospedado
e um único provedor. O resultado será confirmado por webhook verificado, com
persistência, idempotência, deduplicação e testes dos cenários de falha.

O projeto não receberá nem armazenará dados completos de cartão. Também não será
um gateway, cofre, antifraude, ledger contábil, sistema fiscal ou plataforma de
marketplace.

Não criaremos uma abstração ampla para múltiplos provedores antes de concluir e
validar a primeira implementação.

## Consequências

### Positivas

- Menor exposição a dados sensíveis.
- Escopo inicial compreensível e verificável.
- Arquitetura baseada em requisitos reais.
- Possibilidade de validar a experiência antes de extrair pacotes.

### Limitações

- A versão 0.1 não atenderá assinaturas ou marketplaces.
- Trocar de provedor poderá exigir trabalho até que uma segunda implementação
  valide as fronteiras comuns.
- Cada integrador continuará responsável por sua conformidade, operação e
  segurança em produção.

