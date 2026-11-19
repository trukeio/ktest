*** Settings ***
Documentation    Ramps channel 1 of the simulated supply, checking at each level
...              that the terminals measure what was set.

*** Variables ***
${SUPPLY}        psu1.ch1
# Channel 1 drives 10 ohms: 8 V is 0.8 A, inside the 1 A limits.json allows.
@{LEVELS}        2V    4V    6V    8V
# The supply starts with no current limit set, which psusim holds at 0 A.
${CURRENT}       1A

*** Test Cases ***
Ramp
    [Documentation]    Every level within 0.2 V of its setpoint, recorded as a step result.
    [Teardown]    Stress Remove    ${SUPPLY}
    FOR    ${level}    IN    @{LEVELS}
        Apply And Measure    ${level}
    END
    Should Be True    psu1.ch1.out.applied

Over The Limit
    [Documentation]    limits.json caps channel 1 at 12 V. The daemon refuses 14 V,
    ...                and the run fails as refused; the teardown still switches the output off.
    [Teardown]    Stress Remove    ${SUPPLY}
    Apply And Measure    8V
    Stress Apply    ${SUPPLY}    14V    current=${CURRENT}
    Log    not reached

*** Keywords ***
Apply And Measure
    [Arguments]    ${level}
    Stress Apply    ${SUPPLY}    ${level}    current=${CURRENT}
    Wait Until    psu1.ch1.out.applied    timeout=2s
    # The supply is measured every 100 ms; give it two readings.
    Settle    300ms
    ${v}=    Read Channel    psu1.ch1.meas.v
    Should Be Within    ${v}    ${level}    0.2V
    Record Step    ramp    ${level}    ok
