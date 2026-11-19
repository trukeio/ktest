*** Settings ***
Documentation    Powers the simulated DUT, dips its supply to 6 V for 300 ms, and restores it.

*** Test Cases ***
Dip
    [Teardown]    Stress Remove    psu1.ch1
    Stress Apply    psu1.ch1    10V    current=1A
    Settle    1500ms
    Stress Apply    psu1.ch1    6V    current=1A
    Settle    300ms
    Stress Apply    psu1.ch1    10V    current=1A
    Settle    1500ms
