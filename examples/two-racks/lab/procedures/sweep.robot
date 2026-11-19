*** Settings ***
Documentation    Dips the DUT's supply to each level in turn and classifies how the DUT
...              comes through each dip, by ISO 16750-1's functional status (A to E).
...              The DUT's speed is judged in mode active only, so the sweep enters it first.

*** Variables ***
${SUPPLY}      psu1.ch1
${NOMINAL}     13.5V
${CURRENT}     2A
@{LEVELS}      12V    10V    9.5V    8V    6V    3V
${MONITORS}    alive,speed

*** Test Cases ***
Sweep
    [Documentation]    One classification per level, recorded as a step result: the point dip, the level, the class.
    [Teardown]    Stress Remove    ${SUPPLY}
    Stress Apply    ${SUPPLY}    ${NOMINAL}    current=${CURRENT}
    DUT Prepare    active    ready=alive    timeout=5s
    FOR    ${level}    IN    @{LEVELS}
        Verify Baseline    ${MONITORS}    window=300ms
        ${t}=    Mark Time
        Stress Apply    ${SUPPLY}    ${level}    current=${CURRENT}
        Settle    300ms
        Stress Apply    ${SUPPLY}    ${NOMINAL}    current=${CURRENT}
        ${class}=    Classify Recovery    ${MONITORS}    since=${t}    function=alive    action=reset    timeout=2s
        Record Step    dip    ${level}    ${class}
    END

Threshold
    [Documentation]    The lowest dip the DUT comes through untouched, in 0.5 V steps from 12 V.
    [Teardown]    Stress Remove    ${SUPPLY}
    Stress Apply    ${SUPPLY}    ${NOMINAL}    current=${CURRENT}
    DUT Prepare    active    ready=alive    timeout=5s
    Verify Baseline    ${MONITORS}    window=300ms
    ${th}=    Threshold Search    dip    ${MONITORS}    target=${SUPPLY}    nominal=${NOMINAL}
    ...    start=12V    step=0.5V    floor=6V    duration=300ms    recover=2s    action=reset
    Log    ${th}
