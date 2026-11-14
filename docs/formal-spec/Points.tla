-------------------------------- MODULE Points --------------------------------
(***************************************************************************)
(* The points of one customer, as the state of the kitchen service derives  *)
(* them from the log (ADR 22 and ADR 23, internal/orderservice/loyalty.go).  *)
(*                                                                          *)
(* The service decides every command one at a time on a state built from the *)
(* log, and writes the outcome as one record, so the commands below are     *)
(* atomic steps of a single writer; what a model has to show is that no      *)
(* order of them lets a point be spent twice or lets the balance and the     *)
(* history disagree. The log, the other customers, the money and the stock   *)
(* are left out: an order here is only what it did to the points.            *)
(***************************************************************************)
EXTENDS Naturals, Integers, FiniteSets

CONSTANTS Order,            \* the orders of the customer
          MaxUse,           \* the most points one order may use
          MaxEarn,          \* the most points one payment may earn
          MaxAdjust,        \* the most points one change by a manager may give or take
          AllowRefunds,     \* whether paid orders may be cancelled or refunded (a clawback)
          BugNoAvailabilityCheck   \* a placement is not checked against the points that are available

VARIABLES
    balance,        \* what the customer owns, held points included: may go below zero (after a clawback)
    held,           \* set aside for orders that are placed and not paid
    earned, spent,  \* the sums of what orders earned and spent, less what was taken back and given back
    adjusted,       \* the sum of what managers gave, less what they took
    status,         \* of each order
    used, gained,   \* of each order: the points it uses, and the points its payment earned
    clawedBack      \* history: whether a cancellation or a refund ever took earned points back

vars == <<balance, held, earned, spent, adjusted, status, used, gained, clawedBack>>

Init ==
    /\ balance = 0 /\ held = 0 /\ earned = 0 /\ spent = 0 /\ adjusted = 0
    /\ status = [o \in Order |-> "none"]
    /\ used = [o \in Order |-> 0]
    /\ gained = [o \in Order |-> 0]
    /\ clawedBack = FALSE

\* The customer places an order that uses some points. The points are checked against what is available now,
\* which already leaves out what other orders have set aside.
Place(o, n) ==
    /\ status[o] = "none"
    /\ n \in 0..MaxUse
    /\ \/ BugNoAvailabilityCheck
       \/ n <= balance - held
    /\ status' = [status EXCEPT ![o] = "placed"]
    /\ used' = [used EXCEPT ![o] = n]
    /\ held' = held + n
    /\ UNCHANGED <<balance, earned, spent, adjusted, gained, clawedBack>>

\* An order that was not paid is cancelled: its points are let go.
CancelUnpaid(o) ==
    /\ status[o] = "placed"
    /\ status' = [status EXCEPT ![o] = "cancelled"]
    /\ held' = held - used[o]
    /\ UNCHANGED <<balance, earned, spent, adjusted, used, gained, clawedBack>>

\* The payment spends the points that were set aside and earns the points that the service decided.
Pay(o, e) ==
    /\ status[o] = "placed"
    /\ e \in 0..MaxEarn
    /\ status' = [status EXCEPT ![o] = "paid"]
    /\ gained' = [gained EXCEPT ![o] = e]
    /\ held' = held - used[o]
    /\ balance' = balance - used[o] + e
    /\ spent' = spent + used[o]
    /\ earned' = earned + e
    /\ UNCHANGED <<adjusted, used, clawedBack>>

HandOver(o) ==
    /\ status[o] = "paid"
    /\ status' = [status EXCEPT ![o] = "handed"]
    /\ UNCHANGED <<balance, held, earned, spent, adjusted, used, gained, clawedBack>>

\* A paid order is cancelled and all the money goes back: the points it used are given back, the points it
\* earned are taken back.
CancelPaid(o) ==
    /\ AllowRefunds
    /\ status[o] = "paid"
    /\ status' = [status EXCEPT ![o] = "cancelled"]
    /\ balance' = balance + used[o] - gained[o]
    /\ spent' = spent - used[o]
    /\ earned' = earned - gained[o]
    /\ clawedBack' = (clawedBack \/ gained[o] > 0)
    /\ UNCHANGED <<held, adjusted, used, gained>>

\* An order that was handed over is refunded: the points it earned are taken back, and the points it used are not
\* given back (the discount was had, and the money came back).
RefundAfterHandOver(o) ==
    /\ AllowRefunds
    /\ status[o] = "handed"
    /\ status' = [status EXCEPT ![o] = "refunded"]
    /\ balance' = balance - gained[o]
    /\ earned' = earned - gained[o]
    /\ clawedBack' = (clawedBack \/ gained[o] > 0)
    /\ UNCHANGED <<held, spent, adjusted, used, gained>>

\* A manager gives points or takes them off. Points that are set aside cannot be taken.
Adjust(d) ==
    /\ d \in -MaxAdjust..MaxAdjust
    /\ d /= 0
    /\ (d >= 0 \/ -d <= balance - held)
    /\ balance' = balance + d
    /\ adjusted' = adjusted + d
    /\ UNCHANGED <<held, earned, spent, status, used, gained, clawedBack>>

Next ==
    \/ \E o \in Order, n \in 0..MaxUse : Place(o, n)
    \/ \E o \in Order : CancelUnpaid(o) \/ HandOver(o) \/ CancelPaid(o) \/ RefundAfterHandOver(o)
    \/ \E o \in Order, e \in 0..MaxEarn : Pay(o, e)
    \/ \E d \in -MaxAdjust..MaxAdjust : Adjust(d)

Spec == Init /\ [][Next]_vars

\* ---- what is checked ---------------------------------------------------------------------------------------

Sum(f, S) == LET RECURSIVE Go(_)
                 Go(T) == IF T = {} THEN 0 ELSE LET x == CHOOSE x \in T : TRUE IN f[x] + Go(T \ {x})
             IN Go(S)

\* The balance is what was earned, less what was spent, plus what managers changed: nothing else moves it.
BalanceIsTheSumOfItsParts == balance = earned - spent + adjusted

\* What is set aside is exactly the points of the orders that are placed and not paid.
HeldIsWhatPlacedOrdersUse == held = Sum(used, {o \in Order : status[o] = "placed"})

\* The points that are set aside are never more than the customer owns, and the customer never owns less than
\* nothing: unless a clawback took points that were already spent. This is the case that ADR 22 states, and the
\* only one.
NoOverspendUnlessClawedBack == (held > balance \/ balance < 0) => clawedBack

\* The same without refunds at all: no point is ever spent twice, and a balance never goes below zero.
NoOverspend == 0 <= held /\ held <= balance

\* For the model checker only: managers could give points for ever, so the sum of their changes is bounded.
Bounded == adjusted \in -MaxAdjust * 2..MaxAdjust * 2

=============================================================================
