*** Settings ***
Documentation    Powers the DUT at 13.5 V and holds it there, for a rule to end.

*** Test Cases ***
Hold
    [Documentation]    Fails link_lost if the DUT rack stops answering, as the rule removes the stress.
    [Teardown]    Stress Remove    psu1.ch1
    Stress Apply    psu1.ch1    13.5V    current=2A
    Settle    60s
