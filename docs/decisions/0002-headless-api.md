# ADR 0002: API headless como entrega do MVP

- Status: aceito
- Data: 2026-09-05

## Contexto

O público do projeto é formado por pessoas desenvolvedoras e equipes que desejam
integrar seus próprios sistemas a provedores de pagamento. Um frontend de loja
introduziria decisões de produto e interface que não fazem parte do problema
central.

## Decisão

A versão 0.1 será uma API headless. Seu contrato será descrito em OpenAPI e
disponibilizado por Swagger UI, que também funcionará como interface de
demonstração para criar pedidos, iniciar checkouts e consultar estados.

O projeto não terá frontend próprio, carrinho, autenticação de consumidor ou
painel administrativo no MVP. A experiência visual de pagamento será fornecida
pelo checkout hospedado do provedor.

## Consequências

### Positivas

- Escopo menor e alinhado ao público do projeto.
- Contrato consumível por diferentes linguagens e tipos de aplicação.
- Menos dependências e manutenção de interface.
- Documentação testável próxima da implementação.

### Limitações

- O Swagger UI não representa a experiência final do consumidor.
- A URL de checkout ainda precisa ser aberta em um navegador.
- Webhooks assinados não podem ser demonstrados somente pelo formulário do
  Swagger e exigem ferramentas ou fixtures do provedor.
- Quem adota o projeto precisa construir sua própria experiência de produto.

