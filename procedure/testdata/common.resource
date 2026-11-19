*** Keywords ***
Apply And Check
    [Arguments]    ${level}    ${margin}=0.5V
    Stress Apply    ${STRESS}    ${level}
    ${v}=    Read Channel    psu1.ch1.meas.v
    IF    $v > ${margin}
        RETURN    ok
    END
    RETURN    nok

Count
    [Arguments]    ${n}
    IF    $n < 10
        ${m}=    Increment    ${n}
        RETURN    ${m}
    END
    RETURN    ${n}

Increment
    [Arguments]    ${n}
    IF    $n == 0
        RETURN    ${1}
    ELSE IF    $n == 1
        RETURN    ${2}
    ELSE
        RETURN    ${3}
    END
