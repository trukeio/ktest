*** Settings ***
Documentation    Two DUTs on one supply, each with its own DUT rack: one dip stresses both,
...              and each is classified by ISO 16750-1's functional status from its own
...              rack's monitors. dut= says which rack each step is about.

*** Variables ***
${SUPPLY}      psu1.ch1
${NOMINAL}     13.5V
${CURRENT}     2A
@{LEVELS}      12V    9.5V    8V    6V
${MONITORS}    alive,speed

*** Test Cases ***
Pair
    [Documentation]    One classification per DUT per level, recorded at dip-a and dip-b.
    [Teardown]    Stress Remove    ${SUPPLY}
    Stress Apply    ${SUPPLY}    ${NOMINAL}    current=${CURRENT}
    DUT Prepare    active    ready=alive    timeout=5s    dut=a
    DUT Prepare    active    ready=alive    timeout=5s    dut=b
    FOR    ${level}    IN    @{LEVELS}
        Verify Baseline    ${MONITORS}    window=300ms    dut=a
        Verify Baseline    ${MONITORS}    window=300ms    dut=b
        ${t}=    Mark Time
        Stress Apply    ${SUPPLY}    ${level}    current=${CURRENT}
        Settle    300ms
        Stress Apply    ${SUPPLY}    ${NOMINAL}    current=${CURRENT}
        ${a}=    Classify Recovery    ${MONITORS}    since=${t}    function=alive    action=reset    timeout=2s    dut=a
        ${b}=    Classify Recovery    ${MONITORS}    since=${t}    function=alive    action=reset    timeout=2s    dut=b
        Record Step    dip-a    ${level}    ${a}
        Record Step    dip-b    ${level}    ${b}
    END
