*** Settings ***
Documentation    A supply sweep, exercising the subset.
Resource         common.resource
Variables        levels.json

*** Variables ***
${STRESS}        psu1.ch1
@{LEVELS}        10V    11V    12V
&{LIMITS}        low=9V    high=13V

*** Test Cases ***
Sweep
    [Documentation]    Every level, with a check at each.
    [Tags]    sweep
    [Setup]    Log    starting
    [Teardown]    Stress Remove    ${STRESS}
    FOR    ${level}    IN    @{LEVELS}
        ${r}=    Apply And Check    ${level}
        Record Step    sweep    ${level}    ${r}
        IF    $r == "nok"
            BREAK
        ELSE IF    $level >= ${LIMITS.high}
            Fail    never
        ELSE
            CONTINUE
        END
        Fail    not reached
    END
    ${n}=    Set Variable    ${0}
    WHILE    $n < 3    limit=10
        ${n}=    Count    ${n}
    END
    Should Be True    $n == 3
    Should Be True    psu1.ch1.meas.v >= ${LIMITS.low} and psu1.ch1.out.applied
    FOR    ${i}    IN RANGE    2
        Log    ${i}
    END
    Wait Until    psu1.ch1.meas.v > 11.5V    timeout=1s

Fails
    [Teardown]    Stress Remove    ${STRESS}
    Stress Apply    ${STRESS}    ${start}
    Should Be True    psu1.ch1.meas.v > 100V    msg=no hundred volts here

Loops
    WHILE    True    limit=3
        Log    again
    END

Slow
    [Timeout]    50ms
    [Teardown]    Log    cleaned up
    Settle    10s

Refused
    [Teardown]    Log    cleaned up
    Stress Apply    ${STRESS}    12V
    Log    not reached
