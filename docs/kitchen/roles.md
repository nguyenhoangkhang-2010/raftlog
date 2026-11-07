# Roles and permissions

Generated from `internal/kitchen/rbac` by `UPDATE_DOCS=1 go test ./internal/kitchen/rbac`; do not edit by hand.
The test that compares this file with the code fails when they differ.

| Permission | customer | cashier | kitchen | runner | manager | storekeeper | admin |
|---|---|---|---|---|---|---|---|
| `audit.view` |  |  |  |  | ✓ |  | ✓ |
| `menu.manage` |  |  |  |  | ✓ |  |  |
| `order.approve_cancel` |  |  |  |  | ✓ |  |  |
| `order.cancel_unpaid` | ✓ | ✓ |  |  | ✓ |  |  |
| `order.hand_over` |  | ✓ |  | ✓ |  |  |  |
| `order.issue_refund` |  |  |  |  | ✓ |  |  |
| `order.place` | ✓ | ✓ |  |  |  |  |  |
| `order.record_payment` |  | ✓ |  |  |  |  |  |
| `order.request_cancel` | ✓ | ✓ |  |  | ✓ |  |  |
| `order.view_all` |  | ✓ | ✓ | ✓ | ✓ |  |  |
| `order.view_own` | ✓ |  |  |  |  |  |  |
| `promo.manage` |  |  |  |  | ✓ |  |  |
| `report.view` |  |  |  |  | ✓ |  |  |
| `shift.close` |  | ✓ |  |  | ✓ |  |  |
| `shift.open` |  | ✓ |  |  | ✓ |  |  |
| `stock.adjust` |  |  |  |  | ✓ | ✓ |  |
| `stock.receive` |  |  |  |  |  | ✓ |  |
| `stock.view` |  |  | ✓ |  | ✓ | ✓ |  |
| `ticket.finish` |  |  | ✓ |  |  |  |  |
| `ticket.start` |  |  | ✓ |  |  |  |  |
| `user.manage` |  |  |  |  |  |  | ✓ |
