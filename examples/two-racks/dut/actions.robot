*** Settings ***
Documentation    What the DUT rack does to the simulated DUT when the master asks.

*** Variables ***
&{RESET}      Request=1
&{PRESENT}    Active=1

*** Keywords ***
Enter Active
    Start Cyclic    TesterPresent    100ms    ${PRESENT}

Leave Active
    Stop Cyclic    TesterPresent

Reset DUT
    Send Message    DutReset    ${RESET}
    Wait For Monitor    alive    timeout=1s
